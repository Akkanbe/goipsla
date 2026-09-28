# Running goipslad with systemd

`goipslad.service` is a unit that runs goipslad as an unprivileged transient user (`DynamicUser=yes`) with only `CAP_NET_RAW`.

## Installation

In the directory where the release tarball (the output of `make dist`) is extracted:

```sh
sudo install -m 0755 goipslad goipsla /usr/local/bin/
sudo install -m 0644 goipslad.service /etc/systemd/system/
sudo install -d -m 0755 /etc/goipslad
sudo install -m 0644 config.example.yaml /etc/goipslad/config.yaml   # edit before starting
sudo goipsla validate /etc/goipslad/config.yaml
sudo systemctl daemon-reload
sudo systemctl enable --now goipslad
```

The unit has no `Documentation=`. Once the location where the documentation is published is decided, add `Documentation=<URL of docs/operations.md at the published location>` to `[Unit]`.

## Operation

| Task | Command |
|---|---|
| Status and logs | `systemctl status goipslad`, `journalctl -u goipslad -f` |
| Reload the configuration | `sudo systemctl reload goipslad` (SIGHUP; same as `goipsla reload`) |
| Restart | `sudo systemctl restart goipslad` (statistics are lost; like Cisco, they are not persisted) |
| Check status | `sudo goipsla health`, `sudo goipsla show operations` |

If the configuration file has an error, the reload is not applied and the running configuration stays in effect (an error appears in `journalctl`). An error at startup makes startup fail. After editing, check with `goipsla validate` before reloading.

## Privileges (CAP_NET_RAW only)

- The only privilege goipslad needs is `CAP_NET_RAW`. It is used to open raw ICMP sockets and to bind to VRF devices (`SO_BINDTODEVICE`). There is no need to run it as root, and you should not.
- The unit passes only this one capability, with `AmbientCapabilities=CAP_NET_RAW` and `CapabilityBoundingSet=CAP_NET_RAW`. `NoNewPrivileges=yes` prevents gaining any further privileges.
- What was verified (in the staging source container, with privileges dropped by `setpriv` to a non-root user and `CAP_NET_RAW` only):
  - IPv4 / IPv6 Echo, ICMP Timestamp (icmp-jitter), specifying `source-interface` / `source-ip`: works.
  - `vrf:` (`SO_BINDTODEVICE`): binding succeeds, and the destination is looked up in the VRF's routing table.
  - Without `CAP_NET_RAW`: stops at startup with `goipslad: cannot start the ICMP engine (need CAP_NET_RAW): ... operation not permitted`.
- `AF_NETLINK` in `RestrictAddressFamilies` is used to resolve interface names (`source-interface`, IPv6 zones). `AF_UNIX` is used for the API socket, syslog (`/dev/log`), and AgentX over a Unix socket.

## Users who can use goipsla (API socket permissions)

- The API socket is `/run/goipslad/goipslad.sock` (`RuntimeDirectory=goipslad` creates `/run/goipslad`), with permissions 0660.
- **Default** (when `global.api-socket-group` is not set): the socket is owned by goipslad's transient user and its group, so `goipsla` is used as root (`sudo goipsla ...`).
- **To let ordinary users use it**: when you set a group in `global.api-socket-group`, goipslad changes the socket's group to it at startup (chgrp). Users in that group can use `goipsla` without sudo. The steps are as follows.

  ```sh
  sudo groupadd --system goipsla
  sudo usermod -aG goipsla alice          # a user who uses goipsla
  sudo systemctl edit goipslad            # write the following drop-in
  ```

  ```ini
  [Service]
  # The daemon must belong to the group to hand the socket over to it.
  SupplementaryGroups=goipsla
  ```

  ```yaml
  # /etc/goipslad/config.yaml
  global:
    api-socket-group: goipsla
  ```

  ```sh
  sudo systemctl restart goipslad         # api-socket-group is applied at startup (reload only warns)
  ls -l /run/goipslad/goipslad.sock       # srw-rw---- ... goipsla
  ```

- `SupplementaryGroups=goipsla` is needed because a non-root process can change a file's group only to a group it belongs to. If you forget it, goipslad fails to start with `chgrp /run/goipslad/goipslad.sock: operation not permitted`. If the group does not exist, startup also stops with an error.
- The group can be written as a name or as a numeric gid (`global` in [docs/config.md](../../docs/config.md)).

## Adjustments for integrations

The unit's protection settings (`ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, and so on) are fairly strict. For the following integrations, adjust them with a drop-in (`systemctl edit goipslad`).

| Integration | Adjustment |
|---|---|
| An `actions[].exec` script writes to a file | Specify the write destination explicitly, such as `ReadWritePaths=/var/log/ipsla` (with `ProtectSystem=strict`, `/` is read-only) |
| An `actions[].exec` script uses other commands | The script also runs as the transient user. `MemoryDenyWriteExecute=yes` can interfere with interpreters that use JIT (some Python / Node.js), so remove it in that case |
| SNMP (AgentX over a Unix socket) | Set `agentXPerms` on the snmpd side so that goipslad's transient user can write to snmpd's `agentXSocket` socket (for example `agentXPerms 0660 0770 root goipsla` and `SupplementaryGroups=goipsla`). With TCP (`tcp:127.0.0.1:705`), no adjustment is needed |
| syslog | `/dev/log` is not affected by `ProtectSystem`. However, this unit uses a private `/dev` with `PrivateDevices=yes`, so whether `/dev/log` exists in it depends on the systemd version and configuration (not verified; see the note below) |

## Verification

`systemd-analyze verify goipslad.service` passes (when the `ExecStart` executable exists). The `systemd-analyze security` rating is 1.9 (OK).

### Note on using syslog (not verified)

If you enable `global.syslog`, do one of the following. The development environment has no systemd, so it has not been confirmed whether `/dev/log` is created inside the private `/dev` of `PrivateDevices=yes`.

- Check delivery after deployment. Produce threshold / track events, as shown by `goipsla show events`, and check whether they arrive in `journalctl -t goipslad` (or the syslog file). If they do not arrive and goipslad's log shows `syslog not reachable` or `event delivery failed sink=syslog`, apply the following fix.
- Override the unit to set `PrivateDevices=no` (`PrivateDevices=no` under `[Service]` with `systemctl edit goipslad`). Physical devices can stay closed with `DevicePolicy=closed`.
