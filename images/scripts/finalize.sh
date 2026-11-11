#!/bin/bash
# Make the disk a clean, small, identity-free base image.
set -Eeuxo pipefail

# Nothing user-visible may stay root-owned in /home/user.
chown -R user:user /home/user

# cloud-init: done for good. Identity is the guest agent's job from now on.
cloud-init clean --logs --seed || true
touch /etc/cloud/cloud-init.disabled
rm -f /etc/ssh/ssh_host_*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
rm -rf /var/lib/systemd/random-seed /var/lib/systemd/credential.secret

# The firewall defaults that firewall-reset restores on every boot.
rm -f /etc/ufw/*.rules.[0-9]* # backups from ufw reset
tar -C / -cf /usr/local/lib/crowdvm/ufw-default.tar etc/ufw etc/default/ufw

# Caches and logs. apt lists stay so `sudo apt-get install` works at once.
apt-get -y clean
rm -rf /var/cache/apt/*.bin /var/cache/debconf/*-old /var/lib/dpkg/*-old \
	/root/.cache /root/.npm /home/user/.cache /home/user/.npm \
	/home/user/.cargo/registry /home/user/.rustup/downloads /home/user/.rustup/tmp \
	/tmp/* /var/tmp/* /var/crash/* /var/lib/crowdvm-build
find /var/log -type f -delete
rm -rf /var/log/journal/*
rm -f /root/.bash_history /home/user/.bash_history

# Discard free blocks so the host-side qcow2 shrinks (drive has discard=unmap).
sync
fstrim -av
