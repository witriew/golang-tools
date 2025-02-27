// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/gopls/internal/lsp/source"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/xcontext"
)

// workspaceSource reports how the set of active modules has been derived.
type workspaceSource int

const (
	legacyWorkspace     = iota // non-module or single module mode
	goplsModWorkspace          // modules provided by a gopls.mod file
	goWorkWorkspace            // modules provided by a go.work file
	fileSystemWorkspace        // modules found by walking the filesystem
)

func (s workspaceSource) String() string {
	switch s {
	case legacyWorkspace:
		return "legacy"
	case goplsModWorkspace:
		return "gopls.mod"
	case goWorkWorkspace:
		return "go.work"
	case fileSystemWorkspace:
		return "file system"
	default:
		return "!(unknown module source)"
	}
}

// workspaceCommon holds immutable information about the workspace setup.
//
// TODO(rfindley): there is some redundancy here with workspaceInformation.
// Reconcile these two types.
type workspaceCommon struct {
	root        span.URI
	excludePath func(string) bool

	// explicitGowork is, if non-empty, the URI for the explicit go.work file
	// provided via the user's environment.
	explicitGowork span.URI
}

// workspace tracks go.mod files in the workspace, along with the
// gopls.mod file, to provide support for multi-module workspaces.
//
// Specifically, it provides:
//   - the set of modules contained within in the workspace root considered to
//     be 'active'
//   - the workspace modfile, to be used for the go command `-modfile` flag
//   - the set of workspace directories
//
// This type is immutable (or rather, idempotent), so that it may be shared
// across multiple snapshots.
type workspace struct {
	workspaceCommon

	// The source of modules in this workspace.
	moduleSource workspaceSource

	// activeModFiles holds the active go.mod files.
	activeModFiles map[span.URI]struct{}

	// knownModFiles holds the set of all go.mod files in the workspace.
	// In all modes except for legacy, this is equivalent to modFiles.
	knownModFiles map[span.URI]struct{}

	// workFile, if nonEmpty, is the go.work file for the workspace.
	workFile span.URI

	// The workspace module is lazily re-built once after being invalidated.
	// buildMu+built guards this reconstruction.
	//
	// file and wsDirs may be non-nil even if built == false, if they were copied
	// from the previous workspace module version. In this case, they will be
	// preserved if building fails.
	buildMu  sync.Mutex
	built    bool
	buildErr error
	mod      *modfile.File
	sum      []byte
	wsDirs   map[span.URI]struct{}
}

// newWorkspace creates a new workspace at the given root directory,
// determining its module source based on the presence of a gopls.mod or
// go.work file, and the go111moduleOff and useWsModule settings.
//
// If useWsModule is set, the workspace may use a synthetic mod file replacing
// all modules in the root.
//
// If there is no active workspace file (a gopls.mod or go.work), newWorkspace
// scans the filesystem to find modules.
//
// TODO(rfindley): newWorkspace should perhaps never fail, relying instead on
// the criticalError method to surface problems in the workspace.
func newWorkspace(ctx context.Context, root, explicitGowork span.URI, fs source.FileSource, excludePath func(string) bool, go111moduleOff, useWsModule bool) (*workspace, error) {
	ws := &workspace{
		workspaceCommon: workspaceCommon{
			root:           root,
			explicitGowork: explicitGowork,
			excludePath:    excludePath,
		},
	}

	// The user may have a gopls.mod or go.work file that defines their
	// workspace.
	//
	// TODO(rfindley): if GO111MODULE=off, this looks wrong, though there are
	// probably other problems.
	if err := ws.loadExplicitWorkspaceFile(ctx, fs); err == nil {
		return ws, nil
	}

	// Otherwise, in all other modes, search for all of the go.mod files in the
	// workspace.
	knownModFiles, err := findModules(root, excludePath, 0)
	if err != nil {
		return nil, err
	}
	ws.knownModFiles = knownModFiles

	switch {
	case go111moduleOff:
		ws.moduleSource = legacyWorkspace
	case useWsModule:
		ws.activeModFiles = knownModFiles
		ws.moduleSource = fileSystemWorkspace
	default:
		ws.moduleSource = legacyWorkspace
		activeModFiles, err := getLegacyModules(ctx, root, fs)
		if err != nil {
			return nil, err
		}
		ws.activeModFiles = activeModFiles
	}
	return ws, nil
}

// loadExplicitWorkspaceFile loads workspace information from go.work or
// gopls.mod files, setting the active modules, mod file, and module source
// accordingly.
func (ws *workspace) loadExplicitWorkspaceFile(ctx context.Context, fs source.FileSource) error {
	for _, src := range []workspaceSource{goWorkWorkspace, goplsModWorkspace} {
		fh, err := fs.GetFile(ctx, uriForSource(ws.root, ws.explicitGowork, src))
		if err != nil {
			return err
		}
		contents, err := fh.Read()
		if err != nil {
			continue // TODO(rfindley): is it correct to proceed here?
		}
		var file *modfile.File
		var activeModFiles map[span.URI]struct{}
		switch src {
		case goWorkWorkspace:
			file, activeModFiles, err = parseGoWork(ctx, ws.root, fh.URI(), contents, fs)
			ws.workFile = fh.URI()
		case goplsModWorkspace:
			file, activeModFiles, err = parseGoplsMod(ws.root, fh.URI(), contents)
		}
		if err != nil {
			ws.buildMu.Lock()
			ws.built = true
			ws.buildErr = err
			ws.buildMu.Unlock()
		}
		ws.mod = file
		ws.activeModFiles = activeModFiles
		ws.moduleSource = src
		return nil
	}
	return noHardcodedWorkspace
}

var noHardcodedWorkspace = errors.New("no hardcoded workspace")

// TODO(rfindley): eliminate getKnownModFiles.
func (w *workspace) getKnownModFiles() map[span.URI]struct{} {
	return w.knownModFiles
}

// ActiveModFiles returns the set of active mod files for the current workspace.
func (w *workspace) ActiveModFiles() map[span.URI]struct{} {
	return w.activeModFiles
}

// criticalError returns a critical error related to the workspace setup.
func (w *workspace) criticalError(ctx context.Context, fs source.FileSource) (res *source.CriticalError) {
	// For now, we narrowly report errors related to `go.work` files.
	//
	// TODO(rfindley): investigate whether other workspace validation errors
	// can be consolidated here.
	if w.moduleSource == goWorkWorkspace {
		// We should have already built the modfile, but build here to be
		// consistent about accessing w.mod after w.build.
		//
		// TODO(rfindley): build eagerly. Building lazily is a premature
		// optimization that poses a significant burden on the code.
		w.build(ctx, fs)
		if w.buildErr != nil {
			return &source.CriticalError{
				MainError: w.buildErr,
			}
		}
	}
	return nil
}

// modFile gets the workspace modfile associated with this workspace,
// computing it if it doesn't exist.
//
// A fileSource must be passed in to solve a chicken-egg problem: it is not
// correct to pass in the snapshot file source to newWorkspace when
// invalidating, because at the time these are called the snapshot is locked.
// So we must pass it in later on when actually using the modFile.
func (w *workspace) modFile(ctx context.Context, fs source.FileSource) (*modfile.File, error) {
	w.build(ctx, fs)
	return w.mod, w.buildErr
}

func (w *workspace) sumFile(ctx context.Context, fs source.FileSource) ([]byte, error) {
	w.build(ctx, fs)
	return w.sum, w.buildErr
}

func (w *workspace) build(ctx context.Context, fs source.FileSource) {
	w.buildMu.Lock()
	defer w.buildMu.Unlock()

	if w.built {
		return
	}
	// Building should never be cancelled. Since the workspace module is shared
	// across multiple snapshots, doing so would put us in a bad state, and it
	// would not be obvious to the user how to recover.
	ctx = xcontext.Detach(ctx)

	// If the module source is from the filesystem, try to build the workspace
	// module from active modules discovered by scanning the filesystem. Fall
	// back on the pre-existing mod file if parsing fails.
	if w.moduleSource == fileSystemWorkspace {
		file, err := buildWorkspaceModFile(ctx, w.activeModFiles, fs)
		switch {
		case err == nil:
			w.mod = file
		case w.mod != nil:
			// Parsing failed, but we have a previous file version.
			event.Error(ctx, "building workspace mod file", err)
		default:
			// No file to fall back on.
			w.buildErr = err
		}
	}

	if w.mod != nil {
		w.wsDirs = map[span.URI]struct{}{
			w.root: {},
		}
		for _, r := range w.mod.Replace {
			// We may be replacing a module with a different version, not a path
			// on disk.
			if r.New.Version != "" {
				continue
			}
			w.wsDirs[span.URIFromPath(r.New.Path)] = struct{}{}
		}
	}

	// Ensure that there is always at least the root dir.
	if len(w.wsDirs) == 0 {
		w.wsDirs = map[span.URI]struct{}{
			w.root: {},
		}
	}

	sum, err := buildWorkspaceSumFile(ctx, w.activeModFiles, fs)
	if err == nil {
		w.sum = sum
	} else {
		event.Error(ctx, "building workspace sum file", err)
	}

	w.built = true
}

// dirs returns the workspace directories for the loaded modules.
func (w *workspace) dirs(ctx context.Context, fs source.FileSource) []span.URI {
	w.build(ctx, fs)
	var dirs []span.URI
	for d := range w.wsDirs {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i] < dirs[j] })
	return dirs
}

// Clone returns a (possibly) new workspace after invalidating the changed
// files. If w is still valid in the presence of changedURIs, it returns itself
// unmodified.
//
// The returned needReinit flag indicates to the caller that the workspace
// needs to be reinitialized (because a relevant go.mod or go.work file has
// been changed).
//
// TODO(rfindley): it looks wrong that we return 'needReinit' here. The caller
// should determine whether to re-initialize..
func (w *workspace) Clone(ctx context.Context, changes map[span.URI]*fileChange, fs source.FileSource) (_ *workspace, needReinit bool) {
	// Prevent races to w.modFile or w.wsDirs below, if w has not yet been built.
	w.buildMu.Lock()
	defer w.buildMu.Unlock()

	// Clone the workspace. This may be discarded if nothing changed.
	changed := false
	result := &workspace{
		workspaceCommon: w.workspaceCommon,
		moduleSource:    w.moduleSource,
		knownModFiles:   make(map[span.URI]struct{}),
		activeModFiles:  make(map[span.URI]struct{}),
		workFile:        w.workFile,
		mod:             w.mod,
		sum:             w.sum,
		wsDirs:          w.wsDirs,
	}
	for k, v := range w.knownModFiles {
		result.knownModFiles[k] = v
	}
	for k, v := range w.activeModFiles {
		result.activeModFiles[k] = v
	}

	equalURI := func(a, b span.URI) (r bool) {
		// This query is a strange mix of syntax and file system state:
		// deletion of a file causes a false result if the name doesn't change.
		// Our tests exercise only the first clause.
		return a == b || span.SameExistingFile(a, b)
	}

	// First handle changes to the go.work or gopls.mod file. This must be
	// considered before any changes to go.mod or go.sum files, as these files
	// determine which modules we care about. If go.work/gopls.mod has changed
	// we need to either re-read it if it exists or walk the filesystem if it
	// has been deleted. go.work should override the gopls.mod if both exist.
	changed, needReinit = handleWorkspaceFileChanges(ctx, result, changes, fs)
	// Next, handle go.mod changes that could affect our workspace.
	for uri, change := range changes {
		// Otherwise, we only care about go.mod files in the workspace directory.
		if change.isUnchanged || !isGoMod(uri) || !source.InDir(result.root.Filename(), uri.Filename()) {
			continue
		}
		changed = true
		active := result.moduleSource != legacyWorkspace || equalURI(modURI(w.root), uri)
		needReinit = needReinit || (active && change.fileHandle.Saved())
		// Don't mess with the list of mod files if using go.work or gopls.mod.
		if result.moduleSource == goplsModWorkspace || result.moduleSource == goWorkWorkspace {
			continue
		}
		if change.exists {
			result.knownModFiles[uri] = struct{}{}
			if active {
				result.activeModFiles[uri] = struct{}{}
			}
		} else {
			delete(result.knownModFiles, uri)
			delete(result.activeModFiles, uri)
		}
	}

	// Finally, process go.sum changes for any modules that are now active.
	for uri, change := range changes {
		if !isGoSum(uri) {
			continue
		}
		// TODO(rFindley) factor out this URI mangling.
		dir := filepath.Dir(uri.Filename())
		modURI := span.URIFromPath(filepath.Join(dir, "go.mod"))
		if _, active := result.activeModFiles[modURI]; !active {
			continue
		}
		// Only changes to active go.sum files actually cause the workspace to
		// change.
		changed = true
		needReinit = needReinit || change.fileHandle.Saved()
	}

	if !changed {
		return w, false
	}

	return result, needReinit
}

// handleWorkspaceFileChanges handles changes related to a go.work or gopls.mod
// file, updating ws accordingly. ws.root must be set.
func handleWorkspaceFileChanges(ctx context.Context, ws *workspace, changes map[span.URI]*fileChange, fs source.FileSource) (changed, reload bool) {
	if ws.moduleSource != goWorkWorkspace && ws.moduleSource != goplsModWorkspace {
		return false, false
	}

	uri := uriForSource(ws.root, ws.explicitGowork, ws.moduleSource)
	// File opens/closes are just no-ops.
	change, ok := changes[uri]
	if !ok || change.isUnchanged {
		return false, false
	}
	if change.exists {
		// Only invalidate if the file if it actually parses.
		// Otherwise, stick with the current file.
		var parsedFile *modfile.File
		var parsedModules map[span.URI]struct{}
		var err error
		switch ws.moduleSource {
		case goWorkWorkspace:
			parsedFile, parsedModules, err = parseGoWork(ctx, ws.root, uri, change.content, fs)
		case goplsModWorkspace:
			parsedFile, parsedModules, err = parseGoplsMod(ws.root, uri, change.content)
		}
		if err != nil {
			// An unparseable file should not invalidate the workspace:
			// nothing good could come from changing the workspace in
			// this case.
			//
			// TODO(rfindley): well actually, it could potentially lead to a better
			// critical error. Evaluate whether we can unify this case with the
			// error returned by newWorkspace, without needlessly invalidating
			// metadata.
			event.Error(ctx, fmt.Sprintf("parsing %s", filepath.Base(uri.Filename())), err)
		} else {
			// only update the modfile if it parsed.
			changed = true
			reload = change.fileHandle.Saved()
			ws.mod = parsedFile
			ws.knownModFiles = parsedModules
			ws.activeModFiles = make(map[span.URI]struct{})
			for k, v := range parsedModules {
				ws.activeModFiles[k] = v
			}
		}
		return changed, reload
	}
	// go.work/gopls.mod is deleted. We should never see this as the view should have been recreated.
	panic(fmt.Sprintf("internal error: workspace file %q deleted without reinitialization", uri))
}

// goplsModURI returns the URI for the gopls.mod file contained in root.
func uriForSource(root, explicitGowork span.URI, src workspaceSource) span.URI {
	var basename string
	switch src {
	case goplsModWorkspace:
		basename = "gopls.mod"
	case goWorkWorkspace:
		if explicitGowork != "" {
			return explicitGowork
		}
		basename = "go.work"
	default:
		return ""
	}
	return span.URIFromPath(filepath.Join(root.Filename(), basename))
}

// modURI returns the URI for the go.mod file contained in root.
func modURI(root span.URI) span.URI {
	return span.URIFromPath(filepath.Join(root.Filename(), "go.mod"))
}

// isGoMod reports if uri is a go.mod file.
func isGoMod(uri span.URI) bool {
	return filepath.Base(uri.Filename()) == "go.mod"
}

// isGoWork reports if uri is a go.work file.
func isGoWork(uri span.URI) bool {
	return filepath.Base(uri.Filename()) == "go.work"
}

// isGoSum reports if uri is a go.sum or go.work.sum file.
func isGoSum(uri span.URI) bool {
	return filepath.Base(uri.Filename()) == "go.sum" || filepath.Base(uri.Filename()) == "go.work.sum"
}

// fileExists reports if the file uri exists within source.
func fileExists(ctx context.Context, uri span.URI, source source.FileSource) (bool, error) {
	fh, err := source.GetFile(ctx, uri)
	if err != nil {
		return false, err
	}
	return fileHandleExists(fh)
}

// fileHandleExists reports if the file underlying fh actually exits.
func fileHandleExists(fh source.FileHandle) (bool, error) {
	_, err := fh.Read()
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// getLegacyModules returns a module set containing at most the root module.
func getLegacyModules(ctx context.Context, root span.URI, fs source.FileSource) (map[span.URI]struct{}, error) {
	uri := span.URIFromPath(filepath.Join(root.Filename(), "go.mod"))
	modules := make(map[span.URI]struct{})
	exists, err := fileExists(ctx, uri, fs)
	if err != nil {
		return nil, err
	}
	if exists {
		modules[uri] = struct{}{}
	}
	return modules, nil
}

func parseGoWork(ctx context.Context, root, uri span.URI, contents []byte, fs source.FileSource) (*modfile.File, map[span.URI]struct{}, error) {
	workFile, err := modfile.ParseWork(uri.Filename(), contents, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing go.work: %w", err)
	}
	modFiles := make(map[span.URI]struct{})
	for _, dir := range workFile.Use {
		// The resulting modfile must use absolute paths, so that it can be
		// written to a temp directory.
		dir.Path = absolutePath(root, dir.Path)
		modURI := span.URIFromPath(filepath.Join(dir.Path, "go.mod"))
		modFiles[modURI] = struct{}{}
	}

	// TODO(rfindley): we should either not build the workspace modfile here, or
	// not fail so hard. A failure in building the workspace modfile should not
	// invalidate the active module paths extracted above.
	modFile, err := buildWorkspaceModFile(ctx, modFiles, fs)
	if err != nil {
		return nil, nil, err
	}

	// Require a go directive, per the spec.
	if workFile.Go == nil || workFile.Go.Version == "" {
		return nil, nil, fmt.Errorf("go.work has missing or incomplete go directive")
	}
	if err := modFile.AddGoStmt(workFile.Go.Version); err != nil {
		return nil, nil, err
	}

	return modFile, modFiles, nil
}

func parseGoplsMod(root, uri span.URI, contents []byte) (*modfile.File, map[span.URI]struct{}, error) {
	modFile, err := modfile.Parse(uri.Filename(), contents, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing gopls.mod: %w", err)
	}
	modFiles := make(map[span.URI]struct{})
	for _, replace := range modFile.Replace {
		if replace.New.Version != "" {
			return nil, nil, fmt.Errorf("gopls.mod: replaced module %q@%q must not have version", replace.New.Path, replace.New.Version)
		}
		// The resulting modfile must use absolute paths, so that it can be
		// written to a temp directory.
		replace.New.Path = absolutePath(root, replace.New.Path)
		modURI := span.URIFromPath(filepath.Join(replace.New.Path, "go.mod"))
		modFiles[modURI] = struct{}{}
	}
	return modFile, modFiles, nil
}

func absolutePath(root span.URI, path string) string {
	dirFP := filepath.FromSlash(path)
	if !filepath.IsAbs(dirFP) {
		dirFP = filepath.Join(root.Filename(), dirFP)
	}
	return dirFP
}

// errExhausted is returned by findModules if the file scan limit is reached.
var errExhausted = errors.New("exhausted")

// Limit go.mod search to 1 million files. As a point of reference,
// Kubernetes has 22K files (as of 2020-11-24).
const fileLimit = 1000000

// findModules recursively walks the root directory looking for go.mod files,
// returning the set of modules it discovers. If modLimit is non-zero,
// searching stops once modLimit modules have been found.
//
// TODO(rfindley): consider overlays.
func findModules(root span.URI, excludePath func(string) bool, modLimit int) (map[span.URI]struct{}, error) {
	// Walk the view's folder to find all modules in the view.
	modFiles := make(map[span.URI]struct{})
	searched := 0
	errDone := errors.New("done")
	err := filepath.Walk(root.Filename(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Probably a permission error. Keep looking.
			return filepath.SkipDir
		}
		// For any path that is not the workspace folder, check if the path
		// would be ignored by the go command. Vendor directories also do not
		// contain workspace modules.
		if info.IsDir() && path != root.Filename() {
			suffix := strings.TrimPrefix(path, root.Filename())
			switch {
			case checkIgnored(suffix),
				strings.Contains(filepath.ToSlash(suffix), "/vendor/"),
				excludePath(suffix):
				return filepath.SkipDir
			}
		}
		// We're only interested in go.mod files.
		uri := span.URIFromPath(path)
		if isGoMod(uri) {
			modFiles[uri] = struct{}{}
		}
		if modLimit > 0 && len(modFiles) >= modLimit {
			return errDone
		}
		searched++
		if fileLimit > 0 && searched >= fileLimit {
			return errExhausted
		}
		return nil
	})
	if err == errDone {
		return modFiles, nil
	}
	return modFiles, err
}
