// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"io"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
	"golang.org/x/tools/gopls/internal/govulncheck"
	"golang.org/x/tools/gopls/internal/lsp/protocol"
	"golang.org/x/tools/gopls/internal/lsp/safetoken"
	"golang.org/x/tools/gopls/internal/span"
	"golang.org/x/tools/internal/event/label"
	"golang.org/x/tools/internal/event/tag"
	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/packagesinternal"
)

// A GlobalSnapshotID uniquely identifies a snapshot within this process and
// increases monotonically with snapshot creation time.
//
// We use a distinct integral type for global IDs to help enforce correct
// usage.
type GlobalSnapshotID uint64

// Snapshot represents the current state for the given view.
type Snapshot interface {
	// SequenceID is the sequence id of this snapshot within its containing
	// view.
	//
	// Relative to their view sequence ids are monotonically increasing, but this
	// does not hold globally: when new views are created their initial snapshot
	// has sequence ID 0. For operations that span multiple views, use global
	// IDs.
	SequenceID() uint64

	// GlobalID is a globally unique identifier for this snapshot. Global IDs are
	// monotonic: subsequent snapshots will have higher global ID, though
	// subsequent snapshots in a view may not have adjacent global IDs.
	GlobalID() GlobalSnapshotID

	// View returns the View associated with this snapshot.
	View() View

	// BackgroundContext returns a context used for all background processing
	// on behalf of this snapshot.
	BackgroundContext() context.Context

	// Fileset returns the Fileset used to parse all the Go files in this snapshot.
	//
	// If the files are known to belong to a specific Package, use
	// Package.FileSet instead. (We plan to eliminate the
	// Snapshot's cache of parsed files, and thus the need for a
	// snapshot-wide FileSet.)
	FileSet() *token.FileSet

	// ValidBuildConfiguration returns true if there is some error in the
	// user's workspace. In particular, if they are both outside of a module
	// and their GOPATH.
	ValidBuildConfiguration() bool

	// FindFile returns the FileHandle for the given URI, if it is already
	// in the given snapshot.
	FindFile(uri span.URI) VersionedFileHandle

	// GetVersionedFile returns the VersionedFileHandle for a given URI,
	// initializing it if it is not already part of the snapshot.
	GetVersionedFile(ctx context.Context, uri span.URI) (VersionedFileHandle, error)

	// GetFile returns the FileHandle for a given URI, initializing it if it is
	// not already part of the snapshot.
	GetFile(ctx context.Context, uri span.URI) (FileHandle, error)

	// AwaitInitialized waits until the snapshot's view is initialized.
	AwaitInitialized(ctx context.Context)

	// IsOpen returns whether the editor currently has a file open.
	IsOpen(uri span.URI) bool

	// IgnoredFile reports if a file would be ignored by a `go list` of the whole
	// workspace.
	IgnoredFile(uri span.URI) bool

	// Templates returns the .tmpl files
	Templates() map[span.URI]VersionedFileHandle

	// ParseGo returns the parsed AST for the file.
	// If the file is not available, returns nil and an error.
	// Position information is added to FileSet().
	ParseGo(ctx context.Context, fh FileHandle, mode ParseMode) (*ParsedGoFile, error)

	// Analyze runs the specified analyzers on the given package at this snapshot.
	Analyze(ctx context.Context, id PackageID, analyzers []*Analyzer) ([]*Diagnostic, error)

	// RunGoCommandPiped runs the given `go` command, writing its output
	// to stdout and stderr. Verb, Args, and WorkingDir must be specified.
	//
	// RunGoCommandPiped runs the command serially using gocommand.RunPiped,
	// enforcing that this command executes exclusively to other commands on the
	// server.
	RunGoCommandPiped(ctx context.Context, mode InvocationFlags, inv *gocommand.Invocation, stdout, stderr io.Writer) error

	// RunGoCommandDirect runs the given `go` command. Verb, Args, and
	// WorkingDir must be specified.
	RunGoCommandDirect(ctx context.Context, mode InvocationFlags, inv *gocommand.Invocation) (*bytes.Buffer, error)

	// RunGoCommands runs a series of `go` commands that updates the go.mod
	// and go.sum file for wd, and returns their updated contents.
	RunGoCommands(ctx context.Context, allowNetwork bool, wd string, run func(invoke func(...string) (*bytes.Buffer, error)) error) (bool, []byte, []byte, error)

	// RunProcessEnvFunc runs fn with the process env for this snapshot's view.
	// Note: the process env contains cached module and filesystem state.
	RunProcessEnvFunc(ctx context.Context, fn func(*imports.Options) error) error

	// ModFiles are the go.mod files enclosed in the snapshot's view and known
	// to the snapshot.
	ModFiles() []span.URI

	// ParseMod is used to parse go.mod files.
	ParseMod(ctx context.Context, fh FileHandle) (*ParsedModule, error)

	// ModWhy returns the results of `go mod why` for the module specified by
	// the given go.mod file.
	ModWhy(ctx context.Context, fh FileHandle) (map[string]string, error)

	// ModTidy returns the results of `go mod tidy` for the module specified by
	// the given go.mod file.
	ModTidy(ctx context.Context, pm *ParsedModule) (*TidiedModule, error)

	// ModVuln returns import vulnerability analysis for the given go.mod URI.
	// Concurrent requests are combined into a single command.
	ModVuln(ctx context.Context, modURI span.URI) (*govulncheck.Result, error)

	// GoModForFile returns the URI of the go.mod file for the given URI.
	GoModForFile(uri span.URI) span.URI

	// WorkFile, if non-empty, is the go.work file for the workspace.
	WorkFile() span.URI

	// ParseWork is used to parse go.work files.
	ParseWork(ctx context.Context, fh FileHandle) (*ParsedWorkFile, error)

	// BuiltinFile returns information about the special builtin package.
	BuiltinFile(ctx context.Context) (*ParsedGoFile, error)

	// IsBuiltin reports whether uri is part of the builtin package.
	IsBuiltin(ctx context.Context, uri span.URI) bool

	// ReverseDependencies returns a new mapping whose entries are
	// the ID and Metadata of each package in the workspace that
	// directly or transitively depend on the package denoted by id,
	// excluding id itself.
	ReverseDependencies(ctx context.Context, id PackageID, transitive bool) (map[PackageID]*Metadata, error)

	// CachedImportPaths returns all the imported packages loaded in this
	// snapshot, indexed by their package path (not import path, despite the name)
	// and checked in TypecheckWorkspace mode.
	//
	// To reduce latency, it does not wait for type-checking to complete.
	// It is intended for use only in completions.
	CachedImportPaths(ctx context.Context) (map[PackagePath]Package, error)

	// ActiveMetadata returns a new, unordered slice containing
	// metadata for all packages considered 'active' in the workspace.
	//
	// In normal memory mode, this is all workspace packages. In degraded memory
	// mode, this is just the reverse transitive closure of open packages.
	ActiveMetadata(ctx context.Context) ([]*Metadata, error)

	// AllMetadata returns a new unordered array of metadata for all packages in the workspace.
	AllMetadata(ctx context.Context) ([]*Metadata, error)

	// Symbols returns all symbols in the snapshot.
	Symbols(ctx context.Context) map[span.URI][]Symbol

	// Metadata returns the metadata for the specified package,
	// or nil if it was not found.
	Metadata(id PackageID) *Metadata

	// MetadataForFile returns a new slice containing metadata for each
	// package containing the Go file identified by uri, ordered by the
	// number of CompiledGoFiles (i.e. "narrowest" to "widest" package).
	// The result may include tests and intermediate test variants of
	// importable packages.
	// It returns an error if the context was cancelled.
	MetadataForFile(ctx context.Context, uri span.URI) ([]*Metadata, error)

	// TypeCheck parses and type-checks the specified packages,
	// and returns them in the same order as the ids.
	TypeCheck(ctx context.Context, mode TypecheckMode, ids ...PackageID) ([]Package, error)

	// GetCriticalError returns any critical errors in the workspace.
	//
	// A nil result may mean success, or context cancellation.
	GetCriticalError(ctx context.Context) *CriticalError

	// BuildGoplsMod generates a go.mod file for all modules in the workspace.
	// It bypasses any existing gopls.mod.
	BuildGoplsMod(ctx context.Context) (*modfile.File, error)
}

// SnapshotLabels returns a new slice of labels that should be used for events
// related to a snapshot.
func SnapshotLabels(snapshot Snapshot) []label.Label {
	return []label.Label{tag.Snapshot.Of(snapshot.SequenceID()), tag.Directory.Of(snapshot.View().Folder())}
}

// PackageForFile is a convenience function that selects a package to
// which this file belongs (narrowest or widest), type-checks it in
// the requested mode (full or workspace), and returns it, along with
// the parse tree of that file.
//
// Type-checking is expensive. Call snapshot.ParseGo if all you need
// is a parse tree, or snapshot.MetadataForFile if you only need metadata.
func PackageForFile(ctx context.Context, snapshot Snapshot, uri span.URI, mode TypecheckMode, pkgSel PackageSelector) (Package, *ParsedGoFile, error) {
	metas, err := snapshot.MetadataForFile(ctx, uri)
	if err != nil {
		return nil, nil, err
	}
	if len(metas) == 0 {
		return nil, nil, fmt.Errorf("no package metadata for file %s", uri)
	}
	switch pkgSel {
	case NarrowestPackage:
		metas = metas[:1]
	case WidestPackage:
		metas = metas[len(metas)-1:]
	}
	pkgs, err := snapshot.TypeCheck(ctx, mode, metas[0].ID)
	if err != nil {
		return nil, nil, err
	}
	pkg := pkgs[0]
	pgf, err := pkg.File(uri)
	if err != nil {
		return nil, nil, err // "can't happen"
	}
	return pkg, pgf, err
}

// PackageSelector sets how a package is selected out from a set of packages
// containing a given file.
type PackageSelector int

const (
	// NarrowestPackage picks the "narrowest" package for a given file.
	// By "narrowest" package, we mean the package with the fewest number of
	// files that includes the given file. This solves the problem of test
	// variants, as the test will have more files than the non-test package.
	NarrowestPackage PackageSelector = iota

	// WidestPackage returns the Package containing the most files.
	// This is useful for something like diagnostics, where we'd prefer to
	// offer diagnostics for as many files as possible.
	WidestPackage
)

// InvocationFlags represents the settings of a particular go command invocation.
// It is a mode, plus a set of flag bits.
type InvocationFlags int

const (
	// Normal is appropriate for commands that might be run by a user and don't
	// deliberately modify go.mod files, e.g. `go test`.
	Normal InvocationFlags = iota
	// WriteTemporaryModFile is for commands that need information from a
	// modified version of the user's go.mod file, e.g. `go mod tidy` used to
	// generate diagnostics.
	WriteTemporaryModFile
	// LoadWorkspace is for packages.Load, and other operations that should
	// consider the whole workspace at once.
	LoadWorkspace

	// AllowNetwork is a flag bit that indicates the invocation should be
	// allowed to access the network.
	AllowNetwork InvocationFlags = 1 << 10
)

func (m InvocationFlags) Mode() InvocationFlags {
	return m & (AllowNetwork - 1)
}

func (m InvocationFlags) AllowNetwork() bool {
	return m&AllowNetwork != 0
}

// View represents a single workspace.
// This is the level at which we maintain configuration like working directory
// and build tags.
type View interface {
	// Name returns the name this view was constructed with.
	Name() string

	// Folder returns the folder with which this view was created.
	Folder() span.URI

	// Options returns a copy of the Options for this view.
	Options() *Options

	// Snapshot returns the current snapshot for the view, and a
	// release function that must be called when the Snapshot is
	// no longer needed.
	Snapshot(ctx context.Context) (Snapshot, func())

	// IsGoPrivatePath reports whether target is a private import path, as identified
	// by the GOPRIVATE environment variable.
	IsGoPrivatePath(path string) bool

	// ModuleUpgrades returns known module upgrades for the dependencies of
	// modfile.
	ModuleUpgrades(modfile span.URI) map[string]string

	// RegisterModuleUpgrades registers that upgrades exist for the given modules
	// required by modfile.
	RegisterModuleUpgrades(modfile span.URI, upgrades map[string]string)

	// ClearModuleUpgrades clears all upgrades for the modules in modfile.
	ClearModuleUpgrades(modfile span.URI)

	// Vulnerabilites returns known vulnerabilities for the given modfile.
	// TODO(suzmue): replace command.Vuln with a different type, maybe
	// https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck/govulnchecklib#Summary?
	Vulnerabilities(modfile ...span.URI) map[span.URI]*govulncheck.Result

	// SetVulnerabilities resets the list of vulnerabilites that exists for the given modules
	// required by modfile.
	SetVulnerabilities(modfile span.URI, vulncheckResult *govulncheck.Result)

	// FileKind returns the type of a file.
	//
	// We can't reliably deduce the kind from the file name alone,
	// as some editors can be told to interpret a buffer as
	// language different from the file name heuristic, e.g. that
	// an .html file actually contains Go "html/template" syntax,
	// or even that a .go file contains Python.
	FileKind(FileHandle) FileKind

	// GoVersion returns the configured Go version for this view.
	GoVersion() int

	// GoVersionString returns the go version string configured for this view.
	// Unlike [GoVersion], this encodes the minor version and commit hash information.
	GoVersionString() string
}

// A FileSource maps uris to FileHandles. This abstraction exists both for
// testability, and so that algorithms can be run equally on session and
// snapshot files.
type FileSource interface {
	// GetFile returns the FileHandle for a given URI.
	GetFile(ctx context.Context, uri span.URI) (FileHandle, error)
}

// A ParsedGoFile contains the results of parsing a Go file.
type ParsedGoFile struct {
	URI  span.URI
	Mode ParseMode
	File *ast.File
	Tok  *token.File
	// Source code used to build the AST. It may be different from the
	// actual content of the file if we have fixed the AST.
	Src      []byte
	Fixed    bool
	Mapper   *protocol.Mapper
	ParseErr scanner.ErrorList
}

// -- go/token domain convenience helpers --

// Pos returns the token.Pos of protocol position p within the file.
// TODO(adonovan): rename to PositionPos.
func (pgf *ParsedGoFile) Pos(p protocol.Position) (token.Pos, error) {
	offset, err := pgf.Mapper.PositionOffset(p)
	if err != nil {
		return token.NoPos, err
	}
	return safetoken.Pos(pgf.Tok, offset)
}

// PosRange returns a protocol Range for the token.Pos interval in this file.
func (pgf *ParsedGoFile) PosRange(start, end token.Pos) (protocol.Range, error) {
	return pgf.Mapper.PosRange(pgf.Tok, start, end)
}

// NodeRange returns a protocol Range for the ast.Node interval in this file.
func (pgf *ParsedGoFile) NodeRange(node ast.Node) (protocol.Range, error) {
	return pgf.Mapper.NodeRange(pgf.Tok, node)
}

// PosMappedRange returns a MappedRange for the token.Pos interval in this file.
// A MappedRange can be converted to any other form.
func (pgf *ParsedGoFile) PosMappedRange(startPos, endPos token.Pos) (protocol.MappedRange, error) {
	start, end, err := safetoken.Offsets(pgf.Tok, startPos, endPos)
	if err != nil {
		return protocol.MappedRange{}, nil
	}
	return pgf.Mapper.OffsetMappedRange(start, end)
}

// RangeToSpanRange parses a protocol Range back into the go/token domain.
func (pgf *ParsedGoFile) RangeToTokenRange(r protocol.Range) (safetoken.Range, error) {
	start, end, err := pgf.Mapper.RangeOffsets(r)
	if err != nil {
		return safetoken.Range{}, err
	}
	return safetoken.NewRange(pgf.Tok, pgf.Tok.Pos(start), pgf.Tok.Pos(end)), nil
}

// A ParsedModule contains the results of parsing a go.mod file.
type ParsedModule struct {
	URI         span.URI
	File        *modfile.File
	Mapper      *protocol.Mapper
	ParseErrors []*Diagnostic
}

// A ParsedWorkFile contains the results of parsing a go.work file.
type ParsedWorkFile struct {
	URI         span.URI
	File        *modfile.WorkFile
	Mapper      *protocol.Mapper
	ParseErrors []*Diagnostic
}

// A TidiedModule contains the results of running `go mod tidy` on a module.
type TidiedModule struct {
	// Diagnostics representing changes made by `go mod tidy`.
	Diagnostics []*Diagnostic
	// The bytes of the go.mod file after it was tidied.
	TidiedContent []byte
}

// Metadata represents package metadata retrieved from go/packages.
type Metadata struct {
	ID              PackageID
	PkgPath         PackagePath
	Name            PackageName
	GoFiles         []span.URI
	CompiledGoFiles []span.URI
	ForTest         PackagePath // package path under test, or ""
	TypesSizes      types.Sizes
	Errors          []packages.Error
	DepsByImpPath   map[ImportPath]PackageID  // may contain dups; empty ID => missing
	DepsByPkgPath   map[PackagePath]PackageID // values are unique and non-empty
	Module          *packages.Module
	DepsErrors      []*packagesinternal.PackageError

	// Config is the *packages.Config associated with the loaded package.
	Config *packages.Config
}

// IsIntermediateTestVariant reports whether the given package is an
// intermediate test variant, e.g. "net/http [net/url.test]".
//
// Such test variants arise when an x_test package (in this case net/url_test)
// imports a package (in this case net/http) that itself imports the the
// non-x_test package (in this case net/url).
//
// This is done so that the forward transitive closure of net/url_test has
// only one package for the "net/url" import.
// The intermediate test variant exists to hold the test variant import:
//
// net/url_test [net/url.test]
//
//	| "net/http" -> net/http [net/url.test]
//	| "net/url" -> net/url [net/url.test]
//	| ...
//
// net/http [net/url.test]
//
//	| "net/url" -> net/url [net/url.test]
//	| ...
//
// This restriction propagates throughout the import graph of net/http: for
// every package imported by net/http that imports net/url, there must be an
// intermediate test variant that instead imports "net/url [net/url.test]".
//
// As one can see from the example of net/url and net/http, intermediate test
// variants can result in many additional packages that are essentially (but
// not quite) identical. For this reason, we filter these variants wherever
// possible.
func (m *Metadata) IsIntermediateTestVariant() bool {
	return m.ForTest != "" && m.ForTest != m.PkgPath && m.ForTest+"_test" != m.PkgPath
}

// RemoveIntermediateTestVariants removes intermediate test variants, modifying the array.
func RemoveIntermediateTestVariants(metas []*Metadata) []*Metadata {
	res := metas[:0]
	for _, m := range metas {
		if !m.IsIntermediateTestVariant() {
			res = append(res, m)
		}
	}
	return res
}

var ErrViewExists = errors.New("view already exists for session")

// Overlay is the type for a file held in memory on a session.
type Overlay interface {
	Kind() FileKind
	VersionedFileHandle
}

// FileModification represents a modification to a file.
type FileModification struct {
	URI    span.URI
	Action FileAction

	// OnDisk is true if a watched file is changed on disk.
	// If true, Version will be -1 and Text will be nil.
	OnDisk bool

	// Version will be -1 and Text will be nil when they are not supplied,
	// specifically on textDocument/didClose and for on-disk changes.
	Version int32
	Text    []byte

	// LanguageID is only sent from the language client on textDocument/didOpen.
	LanguageID string
}

type FileAction int

const (
	UnknownFileAction = FileAction(iota)
	Open
	Change
	Close
	Save
	Create
	Delete
	InvalidateMetadata
)

func (a FileAction) String() string {
	switch a {
	case Open:
		return "Open"
	case Change:
		return "Change"
	case Close:
		return "Close"
	case Save:
		return "Save"
	case Create:
		return "Create"
	case Delete:
		return "Delete"
	case InvalidateMetadata:
		return "InvalidateMetadata"
	default:
		return "Unknown"
	}
}

var ErrTmpModfileUnsupported = errors.New("-modfile is unsupported for this Go version")
var ErrNoModOnDisk = errors.New("go.mod file is not on disk")

func IsNonFatalGoModError(err error) bool {
	return err == ErrTmpModfileUnsupported || err == ErrNoModOnDisk
}

// ParseMode controls the content of the AST produced when parsing a source file.
type ParseMode int

const (
	// ParseHeader specifies that the main package declaration and imports are needed.
	// This is the mode used when attempting to examine the package graph structure.
	ParseHeader ParseMode = iota

	// ParseExported specifies that the package is used only as a dependency,
	// and only its exported declarations are needed. More may be included if
	// necessary to avoid type errors.
	ParseExported

	// ParseFull specifies the full AST is needed.
	// This is used for files of direct interest where the entire contents must
	// be considered.
	ParseFull
)

// AllParseModes contains all possible values of ParseMode.
// It is used for cache invalidation on a file content change.
var AllParseModes = []ParseMode{ParseHeader, ParseExported, ParseFull}

// TypecheckMode controls what kind of parsing should be done (see ParseMode)
// while type checking a package.
type TypecheckMode int

const (
	// TypecheckFull means to use ParseFull.
	TypecheckFull TypecheckMode = iota
	// TypecheckWorkspace means to use ParseFull for workspace packages, and
	// ParseExported for others.
	TypecheckWorkspace
)

type VersionedFileHandle interface {
	FileHandle
	Version() int32
	Session() string

	// LSPIdentity returns the version identity of a file.
	VersionedFileIdentity() VersionedFileIdentity
}

type VersionedFileIdentity struct {
	URI span.URI

	// SessionID is the ID of the LSP session.
	SessionID string

	// Version is the version of the file, as specified by the client. It should
	// only be set in combination with SessionID.
	Version int32
}

// FileHandle represents a handle to a specific version of a single file.
type FileHandle interface {
	URI() span.URI

	// FileIdentity returns a FileIdentity for the file, even if there was an
	// error reading it.
	FileIdentity() FileIdentity
	// Read reads the contents of a file.
	// If the file is not available, returns a nil slice and an error.
	Read() ([]byte, error)
	// Saved reports whether the file has the same content on disk.
	Saved() bool
}

// A Hash is a cryptographic digest of the contents of a file.
// (Although at 32B it is larger than a 16B string header, it is smaller
// and has better locality than the string header + 64B of hex digits.)
type Hash [sha256.Size]byte

// HashOf returns the hash of some data.
func HashOf(data []byte) Hash {
	return Hash(sha256.Sum256(data))
}

// Hashf returns the hash of a printf-formatted string.
func Hashf(format string, args ...interface{}) Hash {
	// Although this looks alloc-heavy, it is faster than using
	// Fprintf on sha256.New() because the allocations don't escape.
	return HashOf([]byte(fmt.Sprintf(format, args...)))
}

// String returns the digest as a string of hex digits.
func (h Hash) String() string {
	return fmt.Sprintf("%64x", [sha256.Size]byte(h))
}

// Less returns true if the given hash is less than the other.
func (h Hash) Less(other Hash) bool {
	return bytes.Compare(h[:], other[:]) < 0
}

// XORWith updates *h to *h XOR h2.
func (h *Hash) XORWith(h2 Hash) {
	// Small enough that we don't need crypto/subtle.XORBytes.
	for i := range h {
		h[i] ^= h2[i]
	}
}

// FileIdentity uniquely identifies a file at a version from a FileSystem.
type FileIdentity struct {
	URI  span.URI
	Hash Hash // digest of file contents
}

func (id FileIdentity) String() string {
	return fmt.Sprintf("%s%s", id.URI, id.Hash)
}

// FileKind describes the kind of the file in question.
// It can be one of Go,mod, Sum, or Tmpl.
type FileKind int

const (
	// UnknownKind is a file type we don't know about.
	UnknownKind = FileKind(iota)

	// Go is a normal go source file.
	Go
	// Mod is a go.mod file.
	Mod
	// Sum is a go.sum file.
	Sum
	// Tmpl is a template file.
	Tmpl
	// Work is a go.work file.
	Work
)

// Analyzer represents a go/analysis analyzer with some boolean properties
// that let the user know how to use the analyzer.
type Analyzer struct {
	Analyzer *analysis.Analyzer

	// Enabled reports whether the analyzer is enabled. This value can be
	// configured per-analysis in user settings. For staticcheck analyzers,
	// the value of the Staticcheck setting overrides this field.
	//
	// Most clients should use the IsEnabled method.
	Enabled bool

	// Fix is the name of the suggested fix name used to invoke the suggested
	// fixes for the analyzer. It is non-empty if we expect this analyzer to
	// provide its fix separately from its diagnostics. That is, we should apply
	// the analyzer's suggested fixes through a Command, not a TextEdit.
	Fix string

	// ActionKind is the kind of code action this analyzer produces. If
	// unspecified the type defaults to quickfix.
	ActionKind []protocol.CodeActionKind

	// Severity is the severity set for diagnostics reported by this
	// analyzer. If left unset it defaults to Warning.
	Severity protocol.DiagnosticSeverity
}

func (a *Analyzer) String() string { return a.Analyzer.String() }

// IsEnabled reports whether this analyzer is enabled by the given options.
func (a Analyzer) IsEnabled(options *Options) bool {
	// Staticcheck analyzers can only be enabled when staticcheck is on.
	if _, ok := options.StaticcheckAnalyzers[a.Analyzer.Name]; ok {
		if !options.Staticcheck {
			return false
		}
	}
	if enabled, ok := options.Analyses[a.Analyzer.Name]; ok {
		return enabled
	}
	return a.Enabled
}

// Declare explicit types for package paths, names, and IDs to ensure that we
// never use an ID where a path belongs, and vice versa. If we confused these,
// it would result in confusing errors because package IDs often look like
// package paths.
type (
	PackageID   string // go list's unique identifier for a package (e.g. "vendor/example.com/foo [vendor/example.com/bar.test]")
	PackagePath string // name used to prefix linker symbols (e.g. "vendor/example.com/foo")
	PackageName string // identifier in 'package' declaration (e.g. "foo")
	ImportPath  string // path that appears in an import declaration (e.g. "example.com/foo")
)

// Package represents a Go package that has been parsed and type-checked.
// It maintains only the relevant fields of a *go/packages.Package.
type Package interface {
	// Metadata:
	ID() PackageID
	Name() PackageName
	PkgPath() PackagePath
	GetTypesSizes() types.Sizes
	ForTest() string
	Version() *module.Version // may differ from Metadata.Module.Version

	// Results of parsing:
	FileSet() *token.FileSet
	ParseMode() ParseMode
	CompiledGoFiles() []*ParsedGoFile // (borrowed)
	File(uri span.URI) (*ParsedGoFile, error)
	GetSyntax() []*ast.File // (borrowed)
	HasListOrParseErrors() bool

	// Results of type checking:
	GetTypes() *types.Package
	GetTypesInfo() *types.Info
	DirectDep(path PackagePath) (Package, error)
	ResolveImportPath(path ImportPath) (Package, error)
	Imports() []Package // new slice of all direct dependencies, unordered
	HasTypeErrors() bool
	DiagnosticsForFile(uri span.URI) []*Diagnostic                 // new array of list/parse/type errors
	ReferencesTo(PackagePath, objectpath.Path) []protocol.Location // new sorted array of xrefs
}

// A CriticalError is a workspace-wide error that generally prevents gopls from
// functioning correctly. In the presence of critical errors, other diagnostics
// in the workspace may not make sense.
type CriticalError struct {
	// MainError is the primary error. Must be non-nil.
	MainError error

	// Diagnostics contains any supplemental (structured) diagnostics.
	Diagnostics []*Diagnostic
}

// An Diagnostic corresponds to an LSP Diagnostic.
// https://microsoft.github.io/language-server-protocol/specification#diagnostic
type Diagnostic struct {
	URI      span.URI
	Range    protocol.Range
	Severity protocol.DiagnosticSeverity
	Code     string
	CodeHref string

	// Source is a human-readable description of the source of the error.
	// Diagnostics generated by an analysis.Analyzer set it to Analyzer.Name.
	Source DiagnosticSource

	Message string

	Tags    []protocol.DiagnosticTag
	Related []RelatedInformation

	// Fields below are used internally to generate quick fixes. They aren't
	// part of the LSP spec and don't leave the server.
	SuggestedFixes []SuggestedFix
}

func (d *Diagnostic) String() string {
	return fmt.Sprintf("%v: %s", d.Range, d.Message)
}

type DiagnosticSource string

const (
	UnknownError             DiagnosticSource = "<Unknown source>"
	ListError                DiagnosticSource = "go list"
	ParseError               DiagnosticSource = "syntax"
	TypeError                DiagnosticSource = "compiler"
	ModTidyError             DiagnosticSource = "go mod tidy"
	OptimizationDetailsError DiagnosticSource = "optimizer details"
	UpgradeNotification      DiagnosticSource = "upgrade available"
	Vulncheck                DiagnosticSource = "govulncheck"
	TemplateError            DiagnosticSource = "template"
	WorkFileError            DiagnosticSource = "go.work file"
)

func AnalyzerErrorKind(name string) DiagnosticSource {
	return DiagnosticSource(name)
}

// WorkspaceModuleVersion is the nonexistent pseudoversion suffix used in the
// construction of the workspace module. It is exported so that we can make
// sure not to show this version to end users in error messages, to avoid
// confusion.
// The major version is not included, as that depends on the module path.
//
// If workspace module A is dependent on workspace module B, we need our
// nonexistent version to be greater than the version A mentions.
// Otherwise, the go command will try to update to that version. Use a very
// high minor version to make that more likely.
const workspaceModuleVersion = ".9999999.0-goplsworkspace"

func IsWorkspaceModuleVersion(version string) bool {
	return strings.HasSuffix(version, workspaceModuleVersion)
}

func WorkspaceModuleVersion(majorVersion string) string {
	// Use the highest compatible major version to avoid unwanted upgrades.
	// See the comment on workspaceModuleVersion.
	if majorVersion == "v0" {
		majorVersion = "v1"
	}
	return majorVersion + workspaceModuleVersion
}
