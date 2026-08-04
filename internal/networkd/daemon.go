// The daemon orchestrator: build the memberlist config, Create + Join, and run the
// app loop that memberlist does NOT own — scan local ownership, debounce, render,
// whole-table apply, and the ownership_grace GC (spec §6/§9/§16, design §5/§8).
//
// memberlist runs the substrate on its own goroutines (transport, SWIM probe, gossip
// fan-out, push/pull) and calls back into the Delegate/EventDelegate/AliveDelegate
// this package implements. This loop is the small remainder: the timers whose cadence
// is application policy, not gossip mechanics. Everything the loop and the callbacks
// touch on AppliedState is serialized by one mutex, because memberlist calls the
// delegates concurrently with the loop.
package networkd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/frappe/boat/internal/netapply/localownership"
	"github.com/frappe/boat/internal/run"
	"github.com/hashicorp/memberlist"
)

const (
	// leaveTimeout bounds the graceful memberlist.Leave broadcast at shutdown.
	leaveTimeout = 5 * time.Second
	// rejoinInterval retries Join while this host is still alone but has seeds.
	rejoinInterval = 2 * time.Second
	// DefaultStatePath / DefaultStatusPath live under the daemon's state dir.
	DefaultStatePath  = "/var/lib/atlas-networkd/state.json"
	DefaultStatusPath = "/var/lib/atlas-networkd/status.json"
	// DefaultClusterKeyPath is the optional memberlist AES PSK (base64 16/24/32
	// bytes). Absent by default: the ed25519 per-record/per-Meta signatures are the
	// real origin binding (design §8.2); the PSK, if present, adds transport
	// integrity + a join gate on top.
	DefaultClusterKeyPath = "/etc/atlas-networkd/cluster-key"
)

// Config is the full tunable surface. The memberlist timers map the spec §14.3
// ladder onto memberlist's knobs (design §5); the app timers are the cadences
// memberlist does not own; the paths are the on-disk bootstrap + output contract.
type Config struct {
	// memberlist timers (design §5).
	GossipInterval          time.Duration
	GossipNodes             int
	ProbeInterval           time.Duration
	ProbeTimeout            time.Duration
	IndirectChecks          int
	PushPullInterval        time.Duration
	SuspicionMult           int
	SuspicionMaxTimeoutMult int
	RetransmitMult          int
	GossipToTheDeadTime     time.Duration
	ANCPPort                int

	// app-owned timers (spec §11/§16/§14.3).
	OwnershipScanInterval time.Duration
	ApplyDebounce         time.Duration
	GCInterval            time.Duration
	LoopTick              time.Duration
	OwnershipGraceSeconds float64

	SeenCacheSize int
	MeshMTU       int
	WGHostPort    int

	// paths.
	IdentityPath          string
	SeedPath              string
	OperatorPubkeyPath    string
	IntroductionSigPath   string
	WGPrivateKeyPath      string
	WGPublicKeyPath       string
	SigningPrivateKeyPath string
	SigningPublicKeyPath  string
	LocalOwnershipPath    string
	RunConfigPath         string
	StatePath             string
	StatusPath            string
	ConflictsLogPath      string
	ClusterKeyPath        string
}

// DefaultConfig is the spec §14.3 defaults mapped onto memberlist + the app timers.
//
// PushPullInterval is DELIBERATELY not the Python's 1s anti_entropy_interval: a
// memberlist push/pull is a full TCP state sync, not the Python's single-peer pull,
// so the fast path is the broadcast queue (GossipInterval) and push/pull is only the
// backstop — a 15s sync is the right cost for that role. SuspicionMult=4 reproduces
// the ~10s suspect_timeout at moderate fleet size: memberlist's suspicion is
// SuspicionMult*log(N+1)*ProbeInterval, so it scales with cluster size rather than
// being the absolute 10s the Python used (design §5 — document this for WAN fleets).
func DefaultConfig() Config {
	return Config{
		GossipInterval:          200 * time.Millisecond,
		GossipNodes:             3,
		ProbeInterval:           1 * time.Second,
		ProbeTimeout:            500 * time.Millisecond,
		IndirectChecks:          3,
		PushPullInterval:        15 * time.Second,
		SuspicionMult:           4,
		SuspicionMaxTimeoutMult: 6,
		RetransmitMult:          4,
		GossipToTheDeadTime:     30 * time.Second,
		ANCPPort:                7946,

		OwnershipScanInterval: 2 * time.Second,
		ApplyDebounce:         200 * time.Millisecond,
		GCInterval:            1 * time.Second,
		LoopTick:              50 * time.Millisecond,
		OwnershipGraceSeconds: 60,

		SeenCacheSize: 10_000,
		MeshMTU:       defaultMeshMTU,
		WGHostPort:    wireGuardHostPort,

		IdentityPath:          DefaultIdentityPath,
		SeedPath:              DefaultSeedPath,
		OperatorPubkeyPath:    DefaultOperatorPubkeyPath,
		IntroductionSigPath:   DefaultIntroductionSigPath,
		WGPrivateKeyPath:      DefaultWireGuardPrivateKeyPath,
		WGPublicKeyPath:       DefaultWireGuardPublicKeyPath,
		SigningPrivateKeyPath: DefaultSigningPrivateKeyPath,
		SigningPublicKeyPath:  DefaultSigningPublicKeyPath,
		LocalOwnershipPath:    localownership.DefaultPath,
		RunConfigPath:         DefaultRunConfigPath,
		StatePath:             DefaultStatePath,
		StatusPath:            DefaultStatusPath,
		ConflictsLogPath:      DefaultConflictsLog,
		ClusterKeyPath:        DefaultClusterKeyPath,
	}
}

// Daemon owns the applied state, the trust directory, and the memberlist handle. The
// mutex guards every field below it; the fields above it are immutable after Build.
type Daemon struct {
	identity HostIdentity
	config   Config

	wireGuardPublicKey    string
	signingPrivateKey     string
	signingPublicKey      string
	introductionSignature string
	operatorPublicKey     string

	runner    commands
	conflicts *ConflictTracker
	counters  *counters
	nowFn     func() float64

	list          *memberlist.Memberlist
	broadcasts    *memberlist.TransmitLimitedQueue
	joinAddresses []string

	mu             sync.Mutex
	state          *AppliedState
	trust          map[HostID]string
	peerGeneration map[HostID]Generation
	deadAt         map[HostID]float64
	applyDueAt     float64
	lastApplied    string
}

// Build loads identity + keys + seed, restores persisted state, brings the wg-mesh
// device up peer-empty, then creates the memberlist and issues the first join. Every
// bootstrap failure is loud (fail at the boundary): a host that cannot self-identify
// or verify its seed must fail its unit, not come up with a fabricated identity that
// pollutes the cluster.
func Build(ctx context.Context, config Config, runner *run.Runner) (*Daemon, error) {
	identity, err := LoadIdentity(config.IdentityPath)
	if err != nil {
		return nil, err
	}
	if !validMeshAddress(identity.MeshAddress) {
		return nil, fmt.Errorf("identity mesh_address %q is not an IPv6 address", identity.MeshAddress)
	}
	if _, wireGuardPublic, err := EnsureWireGuardKeypair(ctx, runner, config.WGPrivateKeyPath, config.WGPublicKeyPath); err != nil {
		return nil, err
	} else if wireGuardPublic == "" {
		return nil, errors.New("wireguard public key is empty after ensure")
	} else {
		signingPrivate, signingPublic, err := EnsureSigningKeypair(config.SigningPrivateKeyPath, config.SigningPublicKeyPath)
		if err != nil {
			return nil, err
		}
		operatorPublic, err := LoadOperatorPublicKey(config.OperatorPubkeyPath)
		if err != nil {
			return nil, err
		}
		introductionSignature, err := LoadOptionalLine(config.IntroductionSigPath)
		if err != nil {
			return nil, err
		}
		seed, err := LoadSeedOptional(config.SeedPath, operatorPublic)
		if err != nil {
			return nil, err
		}
		daemon := &Daemon{
			identity:              identity,
			config:                config,
			wireGuardPublicKey:    wireGuardPublic,
			signingPrivateKey:     signingPrivate,
			signingPublicKey:      signingPublic,
			introductionSignature: introductionSignature,
			operatorPublicKey:     operatorPublic,
			runner:                runner,
			conflicts:             NewConflictTracker(config.ConflictsLogPath),
			counters:              newCounters(),
			nowFn:                 nowSeconds,
			state:                 NewAppliedState(config.SeenCacheSize),
			trust:                 seed.TrustDirectory(),
			peerGeneration:        map[HostID]Generation{},
			deadAt:                map[HostID]float64{},
		}
		daemon.trust[identity.HostID] = signingPublic // trust our own key so our own alive verifies
		daemon.wireConflictCounters()
		daemon.restorePersistedState()
		daemon.broadcasts = &memberlist.TransmitLimitedQueue{
			NumNodes:       daemon.numMembers,
			RetransmitMult: config.RetransmitMult,
		}
		if err := runBringUp(ctx, runner, identity.MeshAddress, config.MeshMTU); err != nil {
			return nil, fmt.Errorf("wg-mesh bring-up: %w", err)
		}
		list, err := memberlist.Create(daemon.memberlistConfig())
		if err != nil {
			return nil, fmt.Errorf("memberlist create: %w", err)
		}
		daemon.list = list
		daemon.joinAddresses = seed.JoinAddresses(config.ANCPPort)
		if len(daemon.joinAddresses) > 0 {
			if _, err := list.Join(daemon.joinAddresses); err != nil {
				// Not fatal: the rejoin loop retries. A lone host with seeds down comes
				// up empty and converges when a seed returns.
				slog.Warn("atlas-networkd: initial join reached no seeds; will retry", "error", err)
			}
		}
		return daemon, nil
	}
}

// Run drives the app loop until ctx is cancelled, then shuts down gracefully. The loop
// ticks fast and fires each sub-timer on its own cadence off a single float clock, so
// a debounced apply, a scan and a GC never race each other.
func (daemon *Daemon) Run(ctx context.Context) error {
	if _, err := NotifyReady(); err != nil {
		slog.Warn("atlas-networkd: sd_notify READY failed", "error", err)
	}
	daemon.applyNow(ctx) // first render+apply; peer-empty is fine

	ticker := time.NewTicker(daemon.config.LoopTick)
	defer ticker.Stop()
	now := daemon.nowFn()
	nextScan, nextGC, nextRejoin := now, now, now+rejoinInterval.Seconds()

	for {
		select {
		case <-ctx.Done():
			return daemon.shutdown(ctx)
		case <-ticker.C:
			now := daemon.nowFn()
			if now >= nextScan {
				nextScan = now + daemon.config.OwnershipScanInterval.Seconds()
				daemon.scanTick(ctx)
			}
			daemon.applyIfDue(ctx, now)
			if now >= nextGC {
				nextGC = now + daemon.config.GCInterval.Seconds()
				daemon.gcTick(ctx, now)
			}
			if now >= nextRejoin {
				nextRejoin = now + rejoinInterval.Seconds()
				daemon.rejoinIfAlone()
			}
			if _, err := NotifyWatchdog(); err != nil {
				slog.Warn("atlas-networkd: sd_notify WATCHDOG failed", "error", err)
			}
		}
	}
}

// memberlistConfig maps the app config onto memberlist. With no cluster PSK the
// encryption-verify gates MUST be off or an unencrypted cluster cannot talk; the
// ed25519 signatures remain the origin-binding authentication regardless.
func (daemon *Daemon) memberlistConfig() *memberlist.Config {
	config := memberlist.DefaultLANConfig()
	config.Name = daemon.identity.HostID
	config.BindAddr = daemon.identity.Endpoint
	config.BindPort = daemon.config.ANCPPort
	config.AdvertiseAddr = daemon.identity.Endpoint
	config.AdvertisePort = daemon.config.ANCPPort
	config.GossipInterval = daemon.config.GossipInterval
	config.GossipNodes = daemon.config.GossipNodes
	config.ProbeInterval = daemon.config.ProbeInterval
	config.ProbeTimeout = daemon.config.ProbeTimeout
	config.IndirectChecks = daemon.config.IndirectChecks
	config.PushPullInterval = daemon.config.PushPullInterval
	config.SuspicionMult = daemon.config.SuspicionMult
	config.SuspicionMaxTimeoutMult = daemon.config.SuspicionMaxTimeoutMult
	config.RetransmitMult = daemon.config.RetransmitMult
	config.GossipToTheDeadTime = daemon.config.GossipToTheDeadTime
	config.Delegate = daemon
	config.Events = daemon
	config.Alive = daemon

	if key := loadClusterKey(daemon.config.ClusterKeyPath); len(key) > 0 {
		config.SecretKey = key
	} else {
		config.GossipVerifyIncoming = false
		config.GossipVerifyOutgoing = false
	}
	return config
}

// scanTick reads local ownership; on a changed owned set it bumps this host's own
// generation, persists (BEFORE the change can gossip — §12.1/H5, a restart must never
// reuse a generation for different content), signs and applies the advertisement,
// enqueues it for broadcast, and schedules an apply.
func (daemon *Daemon) scanTick(ctx context.Context) {
	owned, err := localownership.Read(daemon.config.LocalOwnershipPath)
	if err != nil {
		daemon.counters.incr("local_ownership_read_failed")
		slog.Error("atlas-networkd: local-ownership read failed", "error", err)
		return
	}
	canonical := sortedUnique(owned)

	daemon.mu.Lock()
	self := daemon.identity.HostID
	if equalStrings(daemon.state.Ownership()[self].Owned, canonical) {
		daemon.mu.Unlock()
		return
	}
	generation := daemon.state.BumpOwnGeneration()
	advertisement := OwningAdvertisement(self, generation, owned)
	signature, err := SignOwnership(advertisement, daemon.signingPrivateKey)
	if err != nil {
		daemon.mu.Unlock()
		slog.Error("atlas-networkd: sign ownership failed", "error", err)
		return
	}
	advertisement.Signature = signature
	daemon.state.ApplyOwnership(advertisement)
	daemon.enqueueOwnershipLocked(advertisement)
	daemon.scheduleApplyLocked(daemon.nowFn())
	daemon.mu.Unlock()

	daemon.persistState()
}

// enqueueOwnershipLocked queues our signed advertisement onto the broadcast queue; a
// NamedBroadcast keyed on origin, so it supersedes our own older queued set.
func (daemon *Daemon) enqueueOwnershipLocked(advertisement OwnershipAdvertisement) {
	message, err := encodeOwnership(advertisement)
	if err != nil {
		slog.Error("atlas-networkd: encode ownership broadcast failed", "error", err)
		return
	}
	daemon.broadcasts.QueueBroadcast(&ownershipBroadcast{origin: advertisement.Origin, message: message})
}

// scheduleApplyLocked arms the debounce: the first change sets a deadline; later
// changes inside the window fold into the same apply (spec §16.4 — without it a /128
// hopping twice yields two syncconfs and a transient invalid state). The caller holds
// the mutex.
func (daemon *Daemon) scheduleApplyLocked(now float64) {
	if daemon.applyDueAt == 0 {
		daemon.applyDueAt = now + daemon.config.ApplyDebounce.Seconds()
	}
}

// applyIfDue runs the apply once the debounce deadline passes.
func (daemon *Daemon) applyIfDue(ctx context.Context, now float64) {
	daemon.mu.Lock()
	due := daemon.applyDueAt != 0 && now >= daemon.applyDueAt
	if due {
		daemon.applyDueAt = 0
	}
	daemon.mu.Unlock()
	if due {
		daemon.applyNow(ctx)
	}
}

// applyNow renders the whole desired config, surfaces conflicts on EVERY recompute
// (a conflict clearing can leave the body identical to a prior no-conflict render,
// so observe before the drift short-circuit), and pushes a whole-table syncconf only
// on drift. The render runs under the mutex (pure, fast); the syncconf does not (it
// shells out).
func (daemon *Daemon) applyNow(ctx context.Context) {
	daemon.mu.Lock()
	self := daemon.identity.HostID
	members := daemon.state.RenderMembers()
	ownership := EffectiveOwnership(daemon.state.Ownership())
	stream := cloneOwnershipStream(daemon.state.Ownership())
	last := daemon.lastApplied
	daemon.mu.Unlock()

	body, renderConflicts, err := RenderWireGuardDesiredWithConflicts(self, members, ownership)
	if err != nil {
		daemon.counters.incr("render_failed")
		slog.Error("atlas-networkd: render failed", "error", err)
		return
	}
	daemon.observeConflicts(ownership, stream, renderConflicts)

	if body == last {
		return
	}
	if err := runApply(ctx, daemon.runner, body, daemon.config.RunConfigPath, daemon.config.WGPrivateKeyPath, daemon.config.WGHostPort); err != nil {
		daemon.counters.incr("apply_failed")
		slog.Error("atlas-networkd: wg-mesh apply failed", "error", err)
		return
	}
	daemon.mu.Lock()
	daemon.lastApplied = body
	daemon.mu.Unlock()
}

// observeConflicts unions the §7.3 owned-/128 double-ownership (origins from the
// per-origin advertisements) with the H2 render-level mesh_address collisions, hands
// the union to the ConflictTracker (which diffs to emit START/END events to
// conflicts.jsonl + the counter), and refreshes status.json.
func (daemon *Daemon) observeConflicts(ownership OwnershipTable, stream map[HostID]OwnershipAdvertisement, renderConflicts map[IP6][]HostID) {
	current := map[IP6][]HostID{}
	for ip := range ownership.Conflicts {
		var origins []HostID
		for origin, advertisement := range stream {
			if advertisement.Owns(ip) {
				origins = append(origins, origin)
			}
		}
		current[ip] = sortedUnique(origins)
	}
	for ip, origins := range renderConflicts {
		current[ip] = sortedUnique(append(current[ip], origins...))
	}
	daemon.conflicts.ObserveConflicts(current)
	daemon.writeStatus(current)
}

// writeStatus atomically refreshes status.json (spec §18.2). Best-effort: a write
// failure is counted and logged, never fatal — the mesh keeps converging even if the
// status surface cannot be written.
func (daemon *Daemon) writeStatus(current map[IP6][]HostID) {
	conflicts := make([]map[string]any, 0, len(current))
	for _, ip := range sortedKeys(current) {
		conflicts = append(conflicts, map[string]any{"private_ip": ip, "origins": current[ip]})
	}
	document := map[string]any{
		"conflict_count": len(current),
		"conflicts":      conflicts,
		"metrics":        daemon.counters.snapshot(),
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err == nil {
		err = atomicWriteFile(daemon.config.StatusPath, append(data, '\n'), 0o644)
	}
	if err != nil {
		daemon.counters.incr("status_write_failed")
		slog.Warn("atlas-networkd: could not write status.json", "path", daemon.config.StatusPath, "error", err)
	}
}

// gcTick reaps ownership past ownership_grace for dead origins, scheduling an apply
// and persisting when anything changed.
func (daemon *Daemon) gcTick(ctx context.Context, now float64) {
	daemon.mu.Lock()
	changed := daemon.reapDeadOriginsLocked(now)
	if changed {
		daemon.scheduleApplyLocked(now)
	}
	daemon.mu.Unlock()
	if changed {
		daemon.persistState()
	}
}

// rejoinIfAlone retries the seed join while this host is still alone but has seeds —
// the rejoin loop that replaces the Python cold-join retry.
func (daemon *Daemon) rejoinIfAlone() {
	if daemon.list == nil || len(daemon.joinAddresses) == 0 {
		return
	}
	if daemon.list.NumMembers() > 1 {
		return
	}
	if _, err := daemon.list.Join(daemon.joinAddresses); err != nil {
		slog.Debug("atlas-networkd: rejoin attempt reached no seeds", "error", err)
	}
}

// numMembers backs the broadcast queue's retransmit math; 1 (just us) before the
// memberlist handle exists.
func (daemon *Daemon) numMembers() int {
	if daemon.list == nil {
		return 1
	}
	return daemon.list.NumMembers()
}

// shutdown does the graceful teardown: broadcast Leave so peers fast-path us, persist
// the generation counter + ownership + TOFU keys, and sd_notify STOPPING so systemd
// reads the exit as clean.
func (daemon *Daemon) shutdown(ctx context.Context) error {
	if daemon.list != nil {
		if err := daemon.list.Leave(leaveTimeout); err != nil {
			slog.Warn("atlas-networkd: memberlist leave failed", "error", err)
		}
		if err := daemon.list.Shutdown(); err != nil {
			slog.Warn("atlas-networkd: memberlist shutdown failed", "error", err)
		}
	}
	daemon.persistState()
	if _, err := NotifyStopping(); err != nil {
		slog.Warn("atlas-networkd: sd_notify STOPPING failed", "error", err)
	}
	return nil
}

// --- state persistence (own_generation + ownership stream + TOFU keys) -----------

// persistedState is the daemon's own crash-recovery shape (NOT a cross-impl contract,
// unlike status.json / conflicts.jsonl). own_generation is the load-bearing field:
// without it a restart would reuse an ownership generation for different content and
// peers would reject the real update as stale.
type persistedState struct {
	OwnGeneration  Generation        `json:"own_generation"`
	Ownership      []ownershipWire   `json:"ownership"`
	SigningPubkeys map[HostID]string `json:"signing_pubkeys"`
}

func (daemon *Daemon) persistState() {
	daemon.mu.Lock()
	document := persistedState{
		OwnGeneration:  daemon.state.OwnGeneration(),
		SigningPubkeys: cloneStringMap(daemon.state.SigningPubkeys()),
	}
	for _, advertisement := range daemon.state.Ownership() {
		document.Ownership = append(document.Ownership, toOwnershipWire(advertisement))
	}
	daemon.mu.Unlock()

	data, err := json.MarshalIndent(document, "", "  ")
	if err == nil {
		err = atomicWriteFile(daemon.config.StatePath, append(data, '\n'), 0o644)
	}
	if err != nil {
		daemon.counters.incr("state_write_failed")
		slog.Warn("atlas-networkd: could not persist state", "path", daemon.config.StatePath, "error", err)
	}
}

// restorePersistedState loads own_generation, the ownership stream, and the TOFU keys
// on boot. A missing file is a clean first boot; a corrupt file is loud (do not paper
// over broken persistence and silently re-advertise generation 1).
func (daemon *Daemon) restorePersistedState() {
	data, err := os.ReadFile(daemon.config.StatePath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		slog.Error("atlas-networkd: could not read persisted state", "path", daemon.config.StatePath, "error", err)
		return
	}
	var document persistedState
	if err := json.Unmarshal(data, &document); err != nil {
		slog.Error("atlas-networkd: persisted state is corrupt", "path", daemon.config.StatePath, "error", err)
		return
	}
	daemon.state.SetOwnGeneration(document.OwnGeneration)
	for _, wire := range document.Ownership {
		daemon.state.ApplyOwnership(fromOwnershipWire(wire))
	}
	for hostID, key := range document.SigningPubkeys {
		if key != "" {
			daemon.state.SigningPubkeys()[hostID] = key
			if _, known := daemon.trust[hostID]; !known {
				daemon.trust[hostID] = key
			}
		}
	}
}

// wireConflictCounters bumps a live conflicts_active gauge on every START/END event.
func (daemon *Daemon) wireConflictCounters() {
	daemon.conflicts.Subscribe(func(event ConflictEvent) {
		if event.Kind == "start" {
			daemon.counters.incr("conflicts_active")
		} else {
			daemon.counters.add("conflicts_active", -1)
		}
	})
}

// --- small helpers ---------------------------------------------------------------

// counters is a tiny name→int64 metric map with its own lock, so the delegates can
// bump it without contending the daemon mutex.
type counters struct {
	mu     sync.Mutex
	values map[string]int64
}

func newCounters() *counters { return &counters{values: map[string]int64{}} }

func (c *counters) incr(name string) { c.add(name, 1) }

func (c *counters) add(name string, delta int64) {
	c.mu.Lock()
	c.values[name] += delta
	c.mu.Unlock()
}

func (c *counters) snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := make(map[string]int64, len(c.values))
	for name, value := range c.values {
		snapshot[name] = value
	}
	return snapshot
}

// nowSeconds is the float monotonic-ish clock the grace + debounce math runs on,
// matching ConflictTracker's and AppliedState.GCOriginIfDead's float second API.
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// equalStrings reports whether two already-sorted string slices are element-equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func cloneStringMap(source map[HostID]string) map[HostID]string {
	clone := make(map[HostID]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneOwnershipStream(source map[HostID]OwnershipAdvertisement) map[HostID]OwnershipAdvertisement {
	clone := make(map[HostID]OwnershipAdvertisement, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// loadClusterKey reads the optional memberlist AES PSK. Absent or malformed → no key
// (returns nil); the ed25519 signatures are the real authentication, so a missing PSK
// is a supported posture, not an error.
func loadClusterKey(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(trimSpace(string(data)))
	if err != nil {
		slog.Warn("atlas-networkd: cluster-key is not valid base64; running without a PSK", "path", path)
		return nil
	}
	switch len(key) {
	case 16, 24, 32:
		return key
	default:
		slog.Warn("atlas-networkd: cluster-key must be 16, 24 or 32 bytes; running without a PSK", "bytes", len(key))
		return nil
	}
}

// atomicWriteFile writes data to path via a tempfile + rename in the same directory,
// creating the directory if missing.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// trimSpace trims surrounding ASCII whitespace without pulling strings into this file
// for one call (keys.go already imports strings; this keeps daemon.go's imports lean).
func trimSpace(value string) string {
	start, end := 0, len(value)
	for start < end && isSpace(value[start]) {
		start++
	}
	for end > start && isSpace(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
