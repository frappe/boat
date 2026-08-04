package image

import "context"

// hostsContent is a minimal /etc/hosts. Per-VM hostname mapping (the 127.0.1.1
// line) is added at provision time, not here (see step 3a.4).
const hostsContent = `127.0.0.1   localhost
::1         localhost ip6-localhost ip6-loopback
fe00::0     ip6-localnet
ff00::0     ip6-mcastprefix
ff02::1     ip6-allnodes
ff02::2     ip6-allrouters
`

// sshdDropIn is the Atlas sshd drop-in — it sorts before 60-cloudimg-settings.conf,
// and first match wins per directive, so these take effect (see step 3a.5).
const sshdDropIn = `# Atlas-managed: enforce key-only SSH. Sorts before 60-cloudimg-settings.conf;
# first match wins per directive, so these take effect.
PasswordAuthentication no
PermitRootLogin prohibit-password
`

// fstabContent is a real /etc/fstab — the shipped one literally says UNCONFIGURED.
// We mount by the LABEL mkfs sets in buildExt4 (atlas-root), stable across copies
// (see step 3a.8).
const fstabContent = `LABEL=atlas-root  /  ext4  defaults,errors=remount-ro  0  1
`

// maskedUnits are boot-blocking units to mask. The cloud image boots into
// cloud-init + systemd-networkd-wait-online + snapd seeding, all of which hang
// indefinitely under Atlas (no datasource, static v6 brought up by
// atlas-network.service, no need for snap). See step 3a.1b.
//
// systemd-resolved is here too: the cloud image enables it and symlinks
// /etc/resolv.conf -> ../run/systemd/resolve/stub-resolv.conf, pointing the system
// resolver at the 127.0.0.53 stub. Atlas brings the network up statically with raw
// `ip` commands and never feeds resolved an upstream (no DHCP, no netplan, no DNS=
// drop-in), so the stub has zero name servers: `dig @2606:4700:4700::1111` works
// but getaddrinfo()/apt fail. We mask resolved and (in 3a.4b) replace the symlink
// with a real file atlas-network.service owns.
var maskedUnits = []string{
	"cloud-init.service",
	"cloud-init-local.service",
	"cloud-config.service",
	"cloud-final.service",
	"systemd-networkd-wait-online.service",
	"snapd.seeded.service",
	"snapd.service",
	"snapd.socket",
	"systemd-resolved.service",
}

// junkUnits are junk units to mask for boot speed. Unlike maskedUnits above, none
// of these BLOCK boot — they run in parallel off the SSH critical path. We mask
// them anyway because on a single-tenant guest VM they are pure overhead: they
// burn CPU/IO during the boot storm (slowing the units that DO gate sshd) and
// inflate time-to-ready. Measured on a real Firecracker boot, leaving them enabled
// cost (in parallel) apport ~17s, ModemManager ~9s, plus multipathd/udisks2/polkit
// and the snapd leaf units the core snapd mask above misses. Verified: masking
// these survives unsquash -> pack -> provision -> boot, and again through a golden
// snapshot -> clone -> boot, with MariaDB/Redis (load-bearing for a site VM) left
// enabled. NOTE: this does NOT approach a ~1s boot on its own — the dominant serial
// gates are apparmor.service (~10s) and the virtio dev-vda/tmpfiles chain; those
// are the next levers and are deliberately NOT touched here.
var junkUnits = []string{
	"apport.service",
	"apport-autoreport.path",
	"apport-autoreport.timer",
	"apport-forward.socket",
	"ModemManager.service",
	"multipathd.service",
	"multipathd.socket",
	"udisks2.service",
	"snapd.apparmor.service",
	"snapd.autoimport.service",
	"snapd.core-fixup.service",
	"snapd.recovery-chooser-trigger.service",
	"snapd.system-shutdown.service",
	"snapd.snap-repair.timer",
	"lxd-installer.socket",
	"polkit.service",
	// pollinate phones entropy.ubuntu.com at boot and dominates userspace boot
	// (~10s). The guest now has virtio-rng (/dev/hwrng) from the host, so the
	// boot-time entropy it seeds is unnecessary. See provision-vm.py "entropy".
	"pollinate.service",
	// unattended-upgrades + apt timers. unattended-upgrades is the fattest userland
	// RSS (~23 MB); worse, auto-apt on a live site VM can restart MariaDB/nginx, hold
	// the dpkg lock, or pull a breaking package under a running site. Version pinning
	// is the golden image's job, not a live guest's.
	"unattended-upgrades.service",
	"apt-daily.service",
	"apt-daily.timer",
	"apt-daily-upgrade.service",
	"apt-daily-upgrade.timer",
	// rsyslog. The server image runs journald AND rsyslog, double-writing every log
	// to /var/log/syslog. journald alone suffices (the minimal image ships no rsyslog
	// and is fine); dropping it saves the second writer fighting MariaDB/nginx for
	// ZFS I/O. journald is capped file-driven in normalizeRootfs.
	"rsyslog.service",
	// storage stack. Verified inert on the guest: one virtio disk, no dmsetup
	// devices, empty /proc/mdstat, no iscsi sessions — LVM/CoW/RAID live on the HOST.
	// (multipathd.service/.socket are already masked above; do NOT duplicate.)
	// Masking these drops idle sockets/timers + a few loaded units.
	"lvm2-monitor.service",
	"blk-availability.service",
	"dm-event.socket",
	"open-iscsi.service",
	"iscsid.socket",
	"mdmonitor.service",
	"mdmonitor-oneshot.service",
	"mdcheck_start.timer",
	"mdcheck_continue.timer",
	// virtual-console plumbing. The guest is reached only over SSH; agetty on tty1 is
	// not a real recovery path, and console-setup/keyboard-setup/setvtrgb configure a
	// physical console that doesn't exist. getty@tty1 is a template instance —
	// masking the instance name stops it; verified it does not resurrect via
	// getty.target.wants (booted VMs answer SSH with these masked, 0 failed).
	"getty@tty1.service",
	"setvtrgb.service",
	"keyboard-setup.service",
	"console-setup.service",
	// cron. The only cron content is distro maintenance we're removing; Frappe's
	// scheduler runs inside bench (RQ), not system cron. Future Pilot OS-level
	// schedules should ship as *.timer units (the image already runs
	// apt/logrotate/fstrim that way), never a resurrected crond.
	"cron.service",
	// cosmetic / telemetry timers. motd-news, update-notifier, fwupd-refresh, Ubuntu
	// Pro (ua-timer), man-db and dpkg-db-backup are pure idle overhead on a
	// single-tenant guest; several phone Canonical at idle. networkd-dispatcher's
	// event hooks are unused (atlas-network owns addressing).
	"motd-news.timer",
	"motd-news.service",
	"update-notifier-download.timer",
	"update-notifier-motd.timer",
	"fwupd-refresh.timer",
	"ua-timer.timer",
	"man-db.timer",
	"dpkg-db-backup.timer",
	"networkd-dispatcher.service",
	// apparmor. Deep profile (tier1f/tier2a): apparmor.service is the #1 unit on the
	// SERIAL boot chain that gates sshd — it compiles all 117 stock profiles into the
	// kernel every boot (~0.45s on an idle core, ~1.1s when it contends with the
	// parallel zfs.ko insert on the single vCPU). NONE of the enforced profiles cover
	// the Pilot stack (nginx/mysqld/mariadb/frappe have no profile); the whole set is
	// for units we've already removed (snapd, ubuntu-pro, man, nvidia). A jailer-
	// isolated single-tenant guest gets its real isolation from the HOST jailer, not
	// in-guest AppArmor. Masking drops apparmor off the critical chain entirely. If a
	// future Pilot runs user-supplied server scripts in-guest, add TARGETED
	// nginx/mariadb profiles rather than resurrecting the stock 117.
	"apparmor.service",
	// systemd-networkd. atlas-network.service (a oneshot, Before=network.target) does
	// 100% of the guest's addressing with raw `ip` commands: eth0 shows `unmanaged` /
	// `Network File: n/a` under networkd, /etc/systemd/network/ is empty, and a live
	// test confirmed stopping networkd left IPv6 up and `ping -6` working. networkd is
	// a ~9 MB idle daemon doing nothing here. NOTE: atlas-network does NOT signal
	// network-online.target; nothing on the Pilot critical path Wants/After that
	// target (verified via the golden serve gate), so masking networkd does not hang
	// any unit. Keep atlas-network.service (the real bring-up) untouched.
	"systemd-networkd.service",
	"systemd-networkd.socket",
	// systemd-udevd. The guest has a FIXED virtio topology and needs no rule-based
	// device management: root mounts via `root=/dev/vda` on the cmdline (no
	// LABEL/UUID probe), the core virtio transport drivers (virtio_blk/net/pci) are
	// BUILT INTO the kernel (no MODALIAS work to bring up disk/net), and /dev/vda +
	// devtmpfs static nodes exist without udev. The two loadable modules we bake
	// (virtio_rng, zfs) load via systemd-modules-load / atlas-zfs-load / explicit
	// modprobe — NOT via udev autoload — so removing udevd orphans no driver. Live
	// test: stopping udevd left /dev/vda, the by-label symlink, and a writable root
	// intact. The hwrng=virtio_rng.0 benchmark marker confirms virtio_rng still binds
	// with udevd gone. Frees ~7 MB + drops the udevd daemon and its trigger scan.
	"systemd-udevd.service",
	"systemd-udevd-kernel.socket",
	"systemd-udevd-control.socket",
	"systemd-udev-trigger.service",
	// plymouth. A boot splash for a physical console — there is none on a headless
	// Firecracker guest, so it renders nothing. plymouth-quit / plymouth-quit-wait sat
	// on the tier1f critical chain; masking removes the whole plymouth-* set off the
	// boot path. (Small in ms once the CPU is freed, but pure dead weight.)
	"plymouth-start.service",
	"plymouth-read-write.service",
	"plymouth-quit.service",
	"plymouth-quit-wait.service",
	// ldconfig.service rebuilds /etc/ld.so.cache at every boot (~0.32 s of CPU that,
	// on the single vCPU, contends with the parallel zfs.ko insert). On an immutable
	// image the shared-library set is fixed at bake time, so we pre-seed ld.so.cache
	// in normalizeRootfs (step 3a.11) and mask the boot-time run — the cache is
	// already current, so the boot-time rebuild is recomputing a known answer.
	"ldconfig.service",
}

// normalizeRootfs strips and neutralizes the Ubuntu cloud image's generic-cloud
// assumptions so every VM boots straight to a clean login under Atlas's
// static-IPv6 / no-first-boot-agent model. Left untouched the image (a) blocks
// boot forever on cloud-init's datasource probe, systemd-networkd-wait-online, and
// snapd seeding; (b) ships identical host keys / machine-id across VMs; (c) trusts
// cloud-init for identity Atlas injects at mount time.
//
// Regression checklist: each step must be a no-op OR a correct strip on the
// current upstream image. If a step's target is absent on a future image, that
// step becomes a documented no-op (the `rm -f` / mask calls stay harmless); it is
// not silently dropped.
func normalizeRootfs(ctx context.Context, cmd commands, root string) error {
	// 3a.1 Kill fcnet — a Firecracker-CI artifact (phantom IPv4/30 from the MAC).
	//      The Ubuntu cloud image has none of these files, so every line is a no-op
	//      today. Kept because `rm -f` is harmless and documents the contract.
	for _, path := range []string{
		root + "/usr/local/bin/fcnet-setup.sh",
		root + "/etc/systemd/system/fcnet.service",
		root + "/etc/systemd/system/sshd.service.wants/fcnet.service",
		root + "/etc/systemd/system/multi-user.target.wants/fcnet.service",
	} {
		if _, err := cmd.Run(ctx, "sudo rm -f {}", path); err != nil {
			return err
		}
	}

	// 3a.1b Neutralize cloud-init and the boot-blocking services. Without this the
	//       guest never reaches a login prompt. We mask the units (symlink to
	//       /dev/null) so they cannot start, and set cloud-init's own disable flag
	//       for good measure. Masking is idempotent and survives a reinstall.
	if err := cmd.InstallDirectory(ctx, root+"/etc/cloud", "0755"); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo touch {}", root+"/etc/cloud/cloud-init.disabled"); err != nil {
		return err
	}
	for _, unit := range maskedUnits {
		if _, err := cmd.Run(ctx, "sudo ln -sf /dev/null {}", root+"/etc/systemd/system/"+unit); err != nil {
			return err
		}
	}

	// 3a.1c Mask the boot-speed junk units (see junkUnits). Same /dev/null symlink
	//       mechanism as the boot-blockers above; the difference is intent — these
	//       don't hang boot, they just burn the boot storm. MariaDB/Redis are
	//       deliberately NOT in the list: a site VM needs them.
	for _, unit := range junkUnits {
		if _, err := cmd.Run(ctx, "sudo ln -sf /dev/null {}", root+"/etc/systemd/system/"+unit); err != nil {
			return err
		}
	}

	// 3a.2 Strip the shipped SSH host keys so every VM doesn't share one identity.
	//      We do NOT rely on first-boot regeneration; provision writes fresh per-VM
	//      host keys into the mounted rootfs. The glob is expanded by the shell so
	//      the rm only deletes what exists (nullglob-safe via `sh -c`).
	sshHostKeyPrefix := root + "/etc/ssh/ssh_host_"
	if _, err := cmd.Shell(ctx, "rm -f {}*_key {}*_key.pub", sshHostKeyPrefix, sshHostKeyPrefix); err != nil {
		return err
	}

	// 3a.3 Force regeneration of machine-id on first boot. systemd repopulates an
	//      empty /etc/machine-id at boot if it is zero bytes (NOT if it is absent —
	//      absent triggers a different code path that breaks journald).
	if _, err := cmd.Run(ctx, "sudo truncate -s 0 {}", root+"/etc/machine-id"); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo rm -f {}", root+"/var/lib/dbus/machine-id"); err != nil {
		return err
	}

	// 3a.4 Normalize /etc/hosts to a minimal template. Overwriting is correct
	//      regardless of what the upstream file contains — Atlas owns it.
	if err := cmd.InstallFile(ctx, hostsContent, root+"/etc/hosts", "0644"); err != nil {
		return err
	}

	// 3a.4b Make /etc/resolv.conf a real, Atlas-owned file. The cloud image ships it
	//       as a symlink to systemd-resolved's stub; resolved is masked in 3a.1b, but
	//       the dangling symlink would still defeat atlas-network.service's
	//       `> /etc/resolv.conf`. Replace it with a regular file carrying the
	//       Cloudflare v6 resolver; the guest unit re-asserts the same line at every
	//       boot. `rm -f` first so InstallFile writes a real file rather than
	//       following the symlink.
	if _, err := cmd.Run(ctx, "sudo rm -f {}", root+"/etc/resolv.conf"); err != nil {
		return err
	}
	if err := cmd.InstallFile(ctx, "nameserver 2606:4700:4700::1111\n", root+"/etc/resolv.conf", "0644"); err != nil {
		return err
	}

	// 3a.5 Lock root password (key-only by contract) and enforce key-only SSH. The
	//      cloud image's sshd_config `Include`s /etc/ssh/sshd_config.d/*.conf and
	//      ships 60-cloudimg-settings.conf enabling PasswordAuthentication. A prepend
	//      would be overridden by that Include, so we drop 00-atlas.conf into the same
	//      directory — it sorts first, and first match wins per directive.
	if _, err := cmd.Run(ctx, "sudo sed -i s|^root:[^:]*:|root:!:| {}", root+"/etc/shadow"); err != nil {
		return err
	}
	if err := cmd.InstallDirectory(ctx, root+"/etc/ssh/sshd_config.d", "0755"); err != nil {
		return err
	}
	if err := cmd.InstallFile(ctx, sshdDropIn, root+"/etc/ssh/sshd_config.d/00-atlas.conf", "0644"); err != nil {
		return err
	}

	// 3a.6 Ensure /home/ubuntu is owned by uid/gid 1000 *if it exists*. The cloud
	//      image does NOT ship /home/ubuntu — cloud-init (masked) creates the ubuntu
	//      user on first boot. This is a guarded no-op today, kept so a future image
	//      that does ship the dir gets correct ownership. The probe runs as root.
	if cmd.OK(ctx, "sudo test -d {}", root+"/home/ubuntu") {
		if _, err := cmd.Run(ctx, "sudo chown -R 1000:1000 {}", root+"/home/ubuntu"); err != nil {
			return err
		}
	}

	// 3a.7 Quieten the motd. 60-unminimize prints a "this image is minimized" nag on
	//      every login; 50-motd-news fetches news from Canonical which on v6-only with
	//      strict resolv.conf hangs briefly.
	if _, err := cmd.Run(
		ctx, "sudo rm -f {} {}",
		root+"/etc/update-motd.d/50-motd-news", root+"/etc/update-motd.d/60-unminimize",
	); err != nil {
		return err
	}

	// 3a.8 Write a real /etc/fstab. The shipped one literally says UNCONFIGURED. The
	//      rootfs UUID is unknown until mkfs runs and stable across copies, so we
	//      mount by the LABEL mkfs sets in buildExt4.
	if err := cmd.InstallFile(ctx, fstabContent, root+"/etc/fstab", "0644"); err != nil {
		return err
	}

	// 3a.9 Drop the PackageKit apt hook. `20packagekit` installs a dpkg Post-Invoke
	//      that `gdbus call`s org.freedesktop.PackageKit after EVERY install. The
	//      guest has dbus but PackageKit is masked/absent, so gdbus D-Bus-activates
	//      it, blocks, hits its own `--timeout 4`, and adds ~7s to every apt run (the
	//      real cause of the "Timeout was reached" reports). Removing the hook takes
	//      an `apt install` from ~8s to ~1s. `rm -f` is a documented no-op if a future
	//      image drops the file.
	if _, err := cmd.Run(ctx, "sudo rm -f {}", root+"/etc/apt/apt.conf.d/20packagekit"); err != nil {
		return err
	}

	// 3a.10 Cap journald. With rsyslog masked (see junkUnits) journald is the sole log
	//        sink; its store lives on ZFS alongside MariaDB, so bound it small to keep
	//        logs from fighting the site workload for pool I/O.
	if err := cmd.InstallDirectory(ctx, root+"/etc/systemd/journald.conf.d", "0755"); err != nil {
		return err
	}
	if err := cmd.InstallFile(
		ctx, "[Journal]\nSystemMaxUse=50M\nRuntimeMaxUse=50M\n",
		root+"/etc/systemd/journald.conf.d/00-atlas.conf", "0644",
	); err != nil {
		return err
	}

	// 3a.11 Pre-seed the dynamic-linker cache so the boot-time ldconfig.service
	//        (masked in junkUnits) has nothing to recompute. `ldconfig -r <root>`
	//        treats the rootfs as / and writes <root>/etc/ld.so.cache from the image's
	//        fixed library set. On an immutable image that set never changes after
	//        bake, so the cache we write here IS the answer the boot-time run would
	//        produce. `-r` (not a chroot) needs no guest ld.so and runs on the host.
	_, err := cmd.Run(ctx, "sudo ldconfig -r {}", root)
	return err
}
