# Sandboxed game servers

[Documentation](README.md) / Sandboxed game servers

None of this is needed to run Valmin. It puts every game server, and the mods running inside
it, behind a second kernel boundary.

A game server already runs locked down: as an unprivileged user, without Linux
capabilities, and seeing only its own files (see
[what a mod can reach](usage.md#what-a-mod-can-reach)). It still shares the host's kernel,
so a mod that exploits a kernel bug can take over the host. A sandboxed runtime closes that
gap:

- **gVisor** (`runsc`) answers the game's system calls with its own kernel, written in Go and
  running in user space. The host kernel sees only a small, filtered set of calls.
- **Kata Containers** runs each game server in its own small virtual machine with its own
  Linux kernel.

A sandbox does not limit the game server's network: a mod can still reach your LAN, the host
and a cloud metadata service until you
[keep game servers off your LAN](advanced-network-setup.md#keep-game-servers-off-your-lan).
Nor does it change what a mod can do to the server it runs in. The panel itself, SteamCMD
downloads and the host data path check keep Docker's default runtime.

## Choose a runtime

|                     | gVisor                                                 | Kata Containers                                                   |
| ------------------- | ------------------------------------------------------ | ----------------------------------------------------------------- |
| Host needs          | x86-64 or ARM64 Linux; works inside a VPS              | Hardware virtualisation: `/dev/kvm`, bare metal or nested virtualisation |
| Isolation           | User-space kernel in front of the host kernel          | A separate guest kernel in a virtual machine                      |
| Memory per server   | About the same as without a sandbox                    | About 0.8 GiB more than without a sandbox, counted in the server's container; ~1.4 GiB more on the host including the virtual machine |
| CPUs                | All the host's, up to the server's CPU limit           | 1 virtual CPU, plus the server's CPU limit when one is set        |

Measured with an unmodded server and the default 4 GiB memory limit, on Docker 29.9 with
cgroup v2, gVisor `release-20260406.0` and Kata Containers 4.3.1 (QEMU). Under both runtimes
the server became ready with and without crossplay, reported CPU, memory and network use
through Docker, stopped on time and saved its world.

Choose gVisor unless you want the stronger boundary of a virtual machine and the host has
memory to spare. A cloud VPS without `/dev/kvm` cannot run Kata.

## Install gVisor

On Debian or Ubuntu, add gVisor's package repository and install `runsc` by following
[gVisor's installation guide](https://gvisor.dev/docs/user_guide/install/); the package
updates with the system. Elsewhere, install your distribution's package (on Arch, `gvisor-bin`
from the AUR) or the release archive from the same guide, keeping its `gvisor-bin` directory
beside `runsc`.

Register it with Docker and reload Docker, which leaves running containers alone:

```sh
sudo runsc install
sudo systemctl reload docker
```

## Install Kata Containers

Check that the host can run virtual machines: `/dev/kvm` must exist, and
`grep -c -w -E 'vmx|svm' /proc/cpuinfo` must print more than zero.

Install the static release from
[Kata's releases](https://github.com/kata-containers/kata-containers/releases), which unpacks
into `/opt/kata`, or your distribution's package (on Arch, `kata-all-bin` from the AUR).
Kata 4 keeps its shim at `/opt/kata/runtime-rs/bin/containerd-shim-kata-v2`. Link it where
containerd's service finds it, then register it in `/etc/docker/daemon.json`, keeping any
keys already there:

```sh
sudo ln -s /opt/kata/runtime-rs/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2
```

```json
{
  "runtimes": {
    "kata": { "runtimeType": "io.containerd.kata.v2" }
  }
}
```

```sh
sudo systemctl reload docker
```

`/opt/kata/bin/kata-ctl check all` on an AMD host warns about a missing `GenuineIntel`, `vmx`
and a `kvmclock_periodic_sync` parameter. Those checks are for Intel; with `svm` in
`/proc/cpuinfo` and the `kvm_amd` module loaded, Kata runs.

### Size Kata servers

A Kata server's virtual machine gets the server's
[memory limit](usage.md#resource-limits) as its memory, and one virtual CPU plus its CPU limit.
Without a CPU limit the whole server, mods included, runs on one CPU, so set a CPU limit on
each server, and plan for about 1.4 GiB more host memory per server than without a sandbox.

## Turn it on

Confirm Docker lists the runtime:

```sh
docker info --format '{{json .Runtimes}}'
```

Then add the runtime's name and the resolvers game servers should use to `deploy/.env`, and
apply the change from `deploy/`:

```sh
VALMIN_GAME_RUNTIME=runsc
VALMIN_GAME_DNS=1.1.1.1,9.9.9.9
```

```sh
docker compose up -d
```

Use `kata` for Kata Containers. On start, the panel runs a container under the runtime on
the game network and refuses to start if Docker cannot run it, naming the runtime in the
error. Each server moves to the runtime on its next start: the panel recreates its container,
and its world, mods and backups are untouched. Until then it keeps its old container, and
**Admin → Diagnostics** flags a server it can see running under another runtime than
`VALMIN_GAME_RUNTIME`. Restart each server, or wait for its next scheduled restart.

**Diagnostics → Run deep checks** then tests what a game server can reach from inside the
sandbox, including **Name lookups from game servers**.

## DNS

A sandbox cannot reach Docker's own resolver, which game containers on `valmin-games` use, so
the panel refuses `VALMIN_GAME_RUNTIME` without `VALMIN_GAME_DNS`. Without working DNS a
server still starts, but crossplay does not, and neither does a mod that looks up a name.

`VALMIN_GAME_DNS` takes up to three IPv4 or IPv6 addresses. The panel writes them to
`game-resolv.conf` in the data directory each time it starts, and mounts that file read-only
as each game server's `/etc/resolv.conf`. A server picks up a change on its next start.

The queries leave from the game server, not the host. If you use
[valmin-isolate](advanced-network-setup.md#keep-game-servers-off-your-lan) and a resolver on
your LAN, such as the router, list that resolver in the rules' `DNS` too.

## Turn it off

Remove `VALMIN_GAME_RUNTIME` and `VALMIN_GAME_DNS` from `deploy/.env`, run
`docker compose up -d`, and restart each server. Each moves back to Docker's default runtime
on its next start.
