package allowlist

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// modulePath prefixes every internal import, and is how an import of Boat's own
// code is told from an import of the standard library or a dependency.
const modulePath = "github.com/frappe/boat/"

// unprivilegedEntryPoints are the packages the two units that run as the `boat`
// user start from. Every function in them is a root, and reachability is
// followed from there.
//
// `boat bootstrap` and `boat reset-server` are still invoked by Atlas over SSH,
// where `sudo` from root is a pass-through that consults no allow-list at all, so
// requiring grants for them would add standing privileges the `boat` user never
// exercises. But the rest of the WO-6 host verbs — `boat snapshot-vm`,
// `provision-vm`, `sync-image` and the others — are NO LONGER SSH-only: the
// daemon runs them in-process behind POST /host-verbs/{verb} (spec/33 §2.4), as
// the boat user, so each is a root too. Those are named in hostVerbEntryPoints
// below, function by function, because the daemon reaches them through a func
// field injected at start-up (cmd/boat's hostVerbRunner) that the static graph
// cannot follow across.
//
// The list is short and it is the thing to change if a unit gains a verb:
//
//	systemd/boat.service          User=boat   ExecStart=boat daemon
//	systemd/boat-networkd.service User=boat   ExecStart=boat networkd
var unprivilegedEntryPoints = []string{
	// `boat daemon`: the HTTP surface, the sweep, and the host scan it runs at
	// start-up. adopt is named here rather than found through api because
	// cmd/boat wires it into the daemon's start-up directly.
	"internal/api",
	"internal/reconcile",
	"internal/adopt",
	// `boat networkd`: its own unit, same user, privileged for wg and ip.
	"internal/networkd",
}

// hostVerbEntryPoints are the internal functions `boat daemon` reaches through
// the host-verb endpoint — the entry point of each verb in cmd/boat's
// servedHostVerbs, one function apiece. They are roots for the same reason the
// packages above are: the daemon runs them as the boat user. They are named
// function-granular, NOT by package, because a host-verb package also holds code
// the daemon does NOT run as boat: `internal/snapshot` holds RestoreVM, the
// firecracker-vm@ ExecStartPre systemd runs as ROOT, and rooting the whole
// package would demand the boat user hold grants for a boot hook it never runs.
//
// The edge from `internal/api` into these cannot be followed statically —
// cmd/boat's hostVerbRunner is set on a Server field at daemon start-up, so
// api.RunHostVerb calls `.Run` on an interface whose implementation lives outside
// internal/ — which is exactly why each is listed here by name.
// The set is the ENABLED host verbs (cmd/boat's servedHostVerbs), and it is these
// six because they reach ZERO privileged command the allow-list does not already
// grant: each shares its host mechanics with a verb the daemon already runs — a
// disk snapshot is the lifecycle LVM path, the memory snapshot is sleep-vm's, host
// keys and firewall and the base-ship cleanup are migration's — so moving them to
// the daemon is "no worse than today" with no new standing privilege (§12).
//
// The heavier verbs (provision-vm, sync-image, promote-snapshot-image, the s3
// backups) are NOT here yet: each needs new grants the boat user does not hold —
// curl, mkfs.ext4, dd, `systemctl start` an arbitrary unit — and those must be
// written SCOPED and proven on a host with `sudo -u boat -n -l` before the daemon
// runs them, or a denied sudo reads back as a fact about the host. vm-tunnel needs
// its wireguard commands literalised first (they render `sudo {}`, which no grant
// can safely authorise). Their transport is ready — run_task will route them the
// day servedHostVerbs and this list gain them together with the grants.
var hostVerbEntryPoints = []string{
	"internal/snapshot.SnapshotVM",
	"internal/snapshot.SnapshotStopVM",
	"internal/snapshot.DeleteSnapshotVM",
	"internal/hostkeys.RegenerateHostKeysVM",
	"internal/netapply/vmnetwork.Firewall",
	"internal/migration.ExportCleanupSource",
	// The read-only sweeps served over /host-reads. They only read nft counters
	// and sleeping markers — the wake trap's own reads, which the daemon already
	// runs — so like the six above they reach no command the boat user lacks.
	"internal/park.PollTraffic",
	"internal/park.Woken",
	// The heavier verbs, each with its own scoped grants in sudoers.d/boat
	// (BOAT_PROVISION / BOAT_WARM_SNAPSHOT / BOAT_PROMOTE_IMAGE / BOAT_SYNC_IMAGE /
	// BOAT_S3_BACKUP / BOAT_VM_TUNNEL). The write halves are pinned; the source
	// globs and open URLs are bounded to /var/lib/atlas and flagged there for the
	// on-host `sudo -u boat -n -l` audit that tightens what shape alone cannot.
	"internal/provision.Provision",
	"internal/snapshot.WarmSnapshotVM",
	"internal/image.PromoteSnapshotImage",
	"internal/image.SyncImage",
	"internal/backup.UploadSnapshotS3",
	"internal/backup.RestoreSnapshotS3",
	"internal/netapply/vmnetwork.Tunnel",
}

// function is one function or method and what this check needs to know about
// it: the privileged commands written inside it, and who it calls.
type function struct {
	templates []Template
	// unreadable are the call sites in this function whose template is computed
	// rather than written, and so cannot be checked against the allow-list.
	unreadable []Template
	calls      []string
}

// DaemonReach returns the templates that run as the `boat` user, found by
// following calls out of the two units' packages.
//
// Function granularity rather than package granularity, and the difference is
// not academic. The daemon reaches `internal/netapply/vmnetwork` through
// exactly one function — `Down`, in a migration phase — while the rest of that
// package is the `boat vm-network-up` hook, which systemd runs as root. Marking
// the whole package as daemon-run would demand grants for every netns, veth and
// tap command it renders, and the `boat` user would end up permanently
// authorised to build networks so that one teardown could run. An allow-list
// that grows to fit the imports is not least privilege.
//
// Names resolve by package and identifier; a method resolves by its name within
// its own package, since the receiver's type is not tracked. That
// over-approximates within a package — two methods sharing a name are one node
// here — and over-approximating is the safe direction: it can ask for a grant
// that is not needed, never miss one that is.
func DaemonReach(repositoryRoot string) (reached []Template, unreadable []Template, err error) {
	functions, err := index(repositoryRoot)
	if err != nil {
		return nil, nil, err
	}
	visited := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		if strings.HasPrefix(name, ambiguousPrefix) {
			for _, candidate := range namesake(functions, strings.TrimPrefix(name, ambiguousPrefix)) {
				visit(candidate)
			}
			return
		}
		current, ok := functions[name]
		if !ok {
			return
		}
		reached = append(reached, current.templates...)
		unreadable = append(unreadable, current.unreadable...)
		for _, called := range current.calls {
			visit(called)
		}
	}
	for name := range functions {
		if isEntryPoint(name) {
			visit(name)
		}
	}
	return reached, unreadable, nil
}

// namesake lists every function that an unresolved `x.method()` could name.
//
// Two rounds, and the second is what makes the daemon's real surface visible.
// The handlers hold their collaborators as interface fields — `server.machines`
// is a `vm.Manager` behind an interface declared in `api` — so `machines.Start`
// names no function in `api` at all, and a search that stopped at the package
// boundary would report the entire VM lifecycle as unreachable and every grant
// that authorises it as dead. When the package has no candidate, the module
// does: any method of that name, wherever it is defined.
func namesake(functions map[string]function, packageAndName string) []string {
	packagePath := packageAndName[:strings.LastIndex(packageAndName, ".")]
	name := packageAndName[strings.LastIndex(packageAndName, ".")+1:]
	candidates := methodsNamed(functions, name, packagePath)
	if len(candidates) == 0 {
		candidates = methodsNamed(functions, name, "")
	}
	return candidates
}

// methodsNamed finds the functions called name, optionally within one package.
func methodsNamed(functions map[string]function, name string, packagePath string) []string {
	var candidates []string
	for candidate := range functions {
		if !strings.HasSuffix(candidate, "."+name) {
			continue
		}
		if packagePath == "" {
			candidates = append(candidates, candidate)
			continue
		}
		if !strings.HasPrefix(candidate, packagePath+".") {
			continue
		}
		// A deeper package's function shares the prefix; the remaining segments
		// tell them apart (`pkg.name` and `pkg.Type.name` are both this package's).
		if strings.Count(strings.TrimPrefix(candidate, packagePath+"."), ".") > 1 {
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func isEntryPoint(qualifiedName string) bool {
	for _, entryPoint := range unprivilegedEntryPoints {
		if strings.HasPrefix(qualifiedName, entryPoint+".") {
			return true
		}
	}
	// The host-verb roots are exact function names, not package prefixes: only
	// `internal/snapshot.SnapshotVM` is a root, not everything in the package.
	for _, entryPoint := range hostVerbEntryPoints {
		if qualifiedName == entryPoint {
			return true
		}
	}
	return false
}

// index reads every function in the module and records what it renders and whom
// it calls.
func index(repositoryRoot string) (map[string]function, error) {
	functions := map[string]function{}
	fileSet := token.NewFileSet()
	root := filepath.Join(repositoryRoot, "internal")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isPortedSource(path) {
			return err
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		packagePath, err := packagePathOf(repositoryRoot, path)
		if err != nil {
			return err
		}
		indexFile(fileSet, file, packagePath, functions)
		return nil
	})
	return functions, err
}

func indexFile(fileSet *token.FileSet, file *ast.File, packagePath string, functions map[string]function) {
	imports := importsOf(file)
	for _, declaration := range file.Decls {
		declared, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := qualify(packagePath, receiverTypeOf(declared), declared.Name.Name)
		current := functions[name]
		literals, dynamic := templatesIn(fileSet, &ast.File{Decls: []ast.Decl{declared}})
		current.templates = append(current.templates, literals...)
		current.unreadable = append(current.unreadable, dynamic...)
		current.calls = append(current.calls, calleesOf(declared, packagePath, imports)...)
		functions[name] = current
	}
}

// qualify names a function the way the graph is keyed. A method carries its
// receiver's type, because two methods of the same name are two functions and
// merging them merges what they reach: `bringDown.run` and `bringUp.run` are
// teardown and bring-up, and collapsing them made the daemon look able to build
// a VM network when all it can do is tear one down.
func qualify(packagePath string, receiverType string, name string) string {
	if receiverType == "" {
		return packagePath + "." + name
	}
	return packagePath + "." + receiverType + "." + name
}

func receiverTypeOf(declared *ast.FuncDecl) string {
	if declared.Recv == nil || len(declared.Recv.List) == 0 {
		return ""
	}
	return typeNameOf(declared.Recv.List[0].Type)
}

// typeNameOf reads a type expression's name, seeing through the pointer.
func typeNameOf(expression ast.Expr) string {
	switch node := expression.(type) {
	case *ast.StarExpr:
		return typeNameOf(node.X)
	case *ast.Ident:
		return node.Name
	}
	return ""
}

// localTypes maps the variables in one function whose type can be read off the
// syntax onto that type's name — the receiver, and the locals built from a
// composite literal or declared with an explicit type.
//
// This is deliberately not a type checker. It resolves the shape this codebase
// actually writes (`bringDown := &bringDown{…}`, then `bringDown.run(ctx)`),
// and anything it cannot read falls back to the ambiguous edge below, which
// over-approximates rather than guessing.
func localTypes(declared *ast.FuncDecl) map[string]string {
	types := map[string]string{}
	if declared.Recv != nil && len(declared.Recv.List) > 0 && len(declared.Recv.List[0].Names) > 0 {
		types[declared.Recv.List[0].Names[0].Name] = receiverTypeOf(declared)
	}
	ast.Inspect(declared, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for index, left := range statement.Lhs {
				name, ok := left.(*ast.Ident)
				if !ok || index >= len(statement.Rhs) {
					continue
				}
				if typeName := compositeTypeOf(statement.Rhs[index]); typeName != "" {
					types[name.Name] = typeName
				}
			}
		case *ast.ValueSpec:
			for _, name := range statement.Names {
				if typeName := typeNameOf(statement.Type); typeName != "" {
					types[name.Name] = typeName
				}
			}
		}
		return true
	})
	return types
}

func compositeTypeOf(expression ast.Expr) string {
	switch node := expression.(type) {
	case *ast.UnaryExpr:
		return compositeTypeOf(node.X)
	case *ast.CompositeLit:
		return typeNameOf(node.Type)
	}
	return ""
}

// importsOf maps the name a file refers to a package by onto that package's
// path, so `vmnetwork.Down` can be resolved to the function it names.
func importsOf(file *ast.File) map[string]string {
	imports := map[string]string{}
	for _, imported := range file.Imports {
		value, err := strconv.Unquote(imported.Path.Value)
		if err != nil || !strings.HasPrefix(value, modulePath) {
			continue
		}
		path := strings.TrimPrefix(value, modulePath)
		name := path[strings.LastIndex(path, "/")+1:]
		if imported.Name != nil {
			name = imported.Name.Name
		}
		imports[name] = path
	}
	return imports
}

// calleesOf lists the functions a declaration calls, by qualified name.
//
// A call it cannot resolve to one function becomes an edge to every method of
// that name in the package. That is the over-approximation, and it is the safe
// direction: it can demand a grant that is not needed, never miss one that is.
func calleesOf(declared *ast.FuncDecl, packagePath string, imports map[string]string) []string {
	types := localTypes(declared)
	var calls []string
	ast.Inspect(declared, func(node ast.Node) bool {
		// A function handed over as a VALUE is an edge too. `vm.New` wires
		// `attachReservedIP: reservedip.Attach` into a field and the verb calls
		// it through that field, so the only syntax naming the reserved-IP NAT
		// anywhere is this reference — and without it the daemon looks unable to
		// reach code it runs on every attach.
		if reference, ok := node.(*ast.SelectorExpr); ok {
			if qualifier, ok := reference.X.(*ast.Ident); ok {
				if imported, ok := imports[qualifier.Name]; ok {
					calls = append(calls, imported+"."+reference.Sel.Name)
				}
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, packagePath+"."+callee.Name)
		case *ast.SelectorExpr:
			qualifier, ok := callee.X.(*ast.Ident)
			if !ok {
				calls = append(calls, ambiguous(packagePath, callee.Sel.Name))
				return true
			}
			if imported, ok := imports[qualifier.Name]; ok {
				calls = append(calls, imported+"."+callee.Sel.Name)
				return true
			}
			if typeName, ok := types[qualifier.Name]; ok {
				calls = append(calls, qualify(packagePath, typeName, callee.Sel.Name))
				return true
			}
			calls = append(calls, ambiguous(packagePath, callee.Sel.Name))
		}
		return true
	})
	return calls
}

// ambiguousPrefix marks an edge that names a method without naming its
// receiver's type. Expanding it happens once the whole index exists.
const ambiguousPrefix = "\x00any."

func ambiguous(packagePath string, name string) string {
	return ambiguousPrefix + packagePath + "." + name
}

// packagePathOf turns a file path into the module-relative package path the
// index is keyed by (`internal/netapply/vmnetwork`).
func packagePathOf(repositoryRoot string, path string) (string, error) {
	relative, err := filepath.Rel(repositoryRoot, filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}
