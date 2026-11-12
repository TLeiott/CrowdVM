#!/bin/bash
# Minimal profile: user, ssh, guest bootstrap unit, network, fast boot.
set -Eeuxo pipefail
here=$(cd "$(dirname "$0")" && pwd)
apt_get() { apt-get -o DPkg::Lock::Timeout=600 -y "$@"; }

# Background apt jobs only get in the way.
systemctl stop unattended-upgrades.service apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service 2>/dev/null || true

# Packages. Purge what slows boot or makes no sense in a sandbox VM.
purge=()
for p in snapd multipath-tools open-iscsi lxd-installer lxd-agent-loader apport apport-symptoms \
	popularity-contest unattended-upgrades motd-news-config landscape-common fwupd packagekit \
	modemmanager pollinate ubuntu-pro-client ubuntu-pro-client-l10n plymouth needrestart udisks2; do
	dpkg -s "$p" >/dev/null 2>&1 && purge+=("$p")
done
[ ${#purge[@]} -eq 0 ] || apt_get purge "${purge[@]}"
rm -rf /snap /var/snap /var/lib/snapd /var/cache/snapd /root/snap
cat >/etc/apt/preferences.d/no-snapd <<'EOF'
Package: snapd
Pin: release a=*
Pin-Priority: -10
EOF
apt_get update
apt_get install --no-install-recommends openssh-server sudo cloud-guest-utils e2fsprogs \
	curl ca-certificates gnupg locales less nano vim-tiny iproute2 iputils-ping ufw \
	cryptsetup-bin fdisk

# Locale (Boat: LANG/LC_ALL en_US.UTF-8).
locale-gen en_US.UTF-8
update-locale LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8

# User "user" (uid 1000), passwordless sudo. Drop the cloud image's default user.
if id ubuntu >/dev/null 2>&1; then userdel -r ubuntu || true; fi
if ! id user >/dev/null 2>&1; then
	useradd -m -u 1000 -U -s /bin/bash -c "CrowdVM user" user
fi
for g in adm sudo kvm systemd-journal; do getent group "$g" >/dev/null && usermod -aG "$g" user; done
passwd -l user
echo 'user ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/90-crowdvm-user
chmod 0440 /etc/sudoers.d/90-crowdvm-user
visudo -cf /etc/sudoers.d/90-crowdvm-user
install -d -m 0700 -o user -g user /home/user/.ssh
chmod 0750 /home/user # other accounts (crowdvm-browser) can't read it

# Files: guest bootstrap unit, netplan, journald caps, grub.
cp -r "$here/../rootfs/minimal/." /
chmod 0600 /etc/netplan/01-crowdvm.yaml
rm -f /etc/netplan/50-cloud-init.yaml
echo 'network: {config: disabled}' >/etc/cloud/cloud.cfg.d/99-crowdvm-no-network.cfg
systemctl enable crowdvm-guest.service

# Firewall (Boat): deny incoming, allow outgoing, ssh + the guest agent port.
# finalize.sh saves them; crowdvm-firewall-reset restores them on every boot.
ufw --force reset >/dev/null
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 7070/tcp
ufw --force enable
systemctl enable crowdvm-firewall-reset.service ufw.service
echo crowdvm >/etc/hostname
sed -i '/^127\.0\.1\.1/d' /etc/hosts && echo '127.0.1.1 crowdvm' >>/etc/hosts

# Fast boot: nothing waits for the network, no periodic jobs fire at boot.
systemctl mask systemd-networkd-wait-online.service NetworkManager-wait-online.service \
	apt-daily.timer apt-daily-upgrade.timer apt-daily.service apt-daily-upgrade.service \
	motd-news.timer motd-news.service man-db.timer e2scrub_all.timer e2scrub_reap.service \
	fstrim.timer dpkg-db-backup.timer sysstat-collect.timer sysstat-summary.timer \
	snapd.seeded.service snapd.service apport.service unattended-upgrades.service \
	ua-timer.timer ua-reboot-cmds.service esm-cache.service lxd-agent.service \
	systemd-pcrphase.service systemd-pcrphase-sysinit.service \
	keyboard-setup.service console-setup.service setvtrgb.service 2>/dev/null || true
systemctl set-default multi-user.target
# Dynamic MOTD scripts that hit the network or are slow.
chmod -x /etc/update-motd.d/* 2>/dev/null || true
# Boot without the initramfs (virtio-blk/scsi + ext4 are built into the kernel;
# saves ~1.6 s). Ubuntu's grub falls back to the initramfs on the next boot if
# an initrd-less boot panics (it passes panic=-1, so don't run qemu -no-reboot).
partuuid=$(blkid -s PARTUUID -o value "$(findmnt -no SOURCE /)")
echo "GRUB_FORCE_PARTUUID=$partuuid" >/etc/default/grub.d/40-force-partuuid.cfg
# Secure layer (v4): PID 1 is preinit, which unlocks the encrypted data
# partition and puts an overlay root on it before systemd starts.
echo 'GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX init=/usr/local/lib/crowdvm/preinit"' >/etc/default/grub.d/60-crowdvm-preinit.cfg
update-grub
