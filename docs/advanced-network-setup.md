# Advanced network setup

[Documentation](README.md) / Advanced network setup

None of this is needed to run Valmin. It covers moving the Docker subnets, keeping the
panel off the internet, and stopping mods from reaching your LAN or the host.

## Change the Docker subnets

Compose reserves `10.89.13.0/24` and `10.89.14.0/24` for the panel's networks and
`10.89.15.0/24` for the game network. If any of them overlaps your LAN, a VPN or
another Docker network, set `VALMIN_SUBNET_PREFIX=172.30` (or any free pair of
octets) in `.env` before the first start; it moves all three subnets and every fixed
address together. Compose does not change the subnet of a network that already exists.
The install script checks for an overlap before it changes anything and takes the same
value as `--subnet-prefix`.

## Restrict the panel's ports

Docker publishes ports through its own firewall rules, so `ufw` does not filter ports
80 and 443 or the game ports. To keep the panel off the internet, do not forward its
ports on the router, or restrict them in the `DOCKER-USER` chain. See
[Docker's packet filtering documentation](https://docs.docker.com/engine/network/packet-filtering-firewalls/).

## Keep game servers off your LAN

A BepInEx mod runs its own code inside the game server, with the network access the
server has. The container itself is locked down (see
[what a mod can reach](usage.md#what-a-mod-can-reach)), but its network is not: by default
a game server can open connections to your router, a NAS, any other device on the LAN,
the host itself, and on a cloud VPS the provider's metadata service at
`169.254.169.254`, which can hand out the machine's credentials.

The rules below drop every connection a game server opens to the host or to a private
address off its own network, while players, the panel's console commands and the server's internet
traffic (Steam, crossplay) keep working. They are
recommended on a cloud VPS and on any LAN with devices you would not hand to a stranger. Game servers can
still reach each other unless you also [filter traffic between game servers](#between-game-servers).

All game servers share the `valmin-games` network,
`${VALMIN_SUBNET_PREFIX}.15.0/24`. The panel joins it at `.15.254` to send console
commands to each server; game ports are published on the host. Connections a server
opens through the host, including to ports the host publishes for containers such as the
panel's HTTPS port, are filtered in Docker's `DOCKER-USER` chain. Connections to the
host's own services (SSH, anything listening on its LAN IP or on the network gateway
`.15.1`) go through `INPUT`. Each gets its own chain.

### The rules

Save this as `/usr/local/sbin/valmin-isolate` and make it executable
(`sudo chmod 755 /usr/local/sbin/valmin-isolate`). It reads the subnet from the
`valmin-games` network itself, so it follows `VALMIN_SUBNET_PREFIX` without being told.
Set `DNS` as described under [DNS](#dns).

```sh
#!/bin/sh
# valmin-isolate start|stop: keep Valmin game servers off the LAN and the host.
set -eu

DNS=""   # resolvers game servers may query, space-separated; see DNS below

ipt() { iptables -w "$@"; }
chain() { ipt -N "$1" 2>/dev/null || ipt -F "$1"; }
jump() { ipt -C "$1" -j "$2" 2>/dev/null || ipt -I "$1" 1 -j "$2"; }
unjump() { while ipt -D "$1" -j "$2" 2>/dev/null; do :; done; }
remove() { ipt -F "$1" 2>/dev/null && ipt -X "$1" || true; }

# The game network's subnet, such as 10.89.15.0/24; the panel holds its .254.
subnet() {
	S=$(docker network inspect valmin-games -f '{{(index .IPAM.Config 0).Subnet}}')
	case "$S" in
	*.0/24) PANEL=${S%.0/24}.254 ;;
	*) echo "valmin-isolate: unexpected valmin-games subnet '$S'" >&2; exit 1 ;;
	esac
	PRIVATE=$S,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10
}

# allow CHAIN: the panel, replies, and DNS to the listed resolvers.
allow() {
	ipt -A "$1" -s "$PANEL" -j RETURN
	ipt -A "$1" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
	for d in $DNS; do
		ipt -A "$1" -s "$S" -d "$d" -p udp --dport 53 -j RETURN
		ipt -A "$1" -s "$S" -d "$d" -p tcp --dport 53 -j RETURN
	done
}

case "${1:-}" in
start)
	subnet
	# Connections game servers open to the host itself.
	chain VALMIN-GAMES-IN
	allow VALMIN-GAMES-IN
	ipt -A VALMIN-GAMES-IN -s "$S" -j DROP
	jump INPUT VALMIN-GAMES-IN

	# Connections game servers open through the host.
	chain VALMIN-GAMES
	allow VALMIN-GAMES
	ipt -A VALMIN-GAMES -s "$S" -d "$PRIVATE" -j DROP
	jump DOCKER-USER VALMIN-GAMES
	;;
stop)
	unjump DOCKER-USER VALMIN-GAMES
	unjump INPUT VALMIN-GAMES-IN
	remove VALMIN-GAMES
	remove VALMIN-GAMES-IN
	;;
*)
	echo "usage: $0 start|stop" >&2
	exit 2
	;;
esac
```

What each rule does, in order:

- The panel's own traffic is exempt: it sends console commands into the game network,
  and its outbound connections may leave through that network too.
  <!-- UNVERIFIED: which of its three networks the panel uses as its default route. If it is not valmin-games, this exemption only matters for console commands. -->
- Replies to connections someone else opened are let through: players joining from the
  LAN, and the panel's console connections.
- Game servers may query the DNS servers you list in `DNS`; see [DNS](#dns).
- Anything else a game server opens to a private, link-local or carrier-grade NAT
  address is dropped.
- On `INPUT`, a game server cannot open any connection to the host.

The rules use `DROP`, not `REJECT`: a rejected connection reads as refused, which looks
the same as a device that answered. If a step fails, `start` stops with an error and the
chain may be left half filled, without its final drop; fix the cause and run it again.
Run it once by hand to try it:

```sh
sudo /usr/local/sbin/valmin-isolate start
sudo iptables -S VALMIN-GAMES
```

### Between game servers

Connections between two game servers stay on the `valmin-games` bridge and do not pass
through the host's firewall unless the `br_netfilter` module is loaded. Docker loads it
only for some network settings; `lsmod | grep br_netfilter` shows whether it is. To drop
these connections too, so that only the panel can reach a server's console port, load it
and keep it loaded:

```sh
echo br_netfilter | sudo tee /etc/modules-load.d/br_netfilter.conf
echo 'net.bridge.bridge-nf-call-iptables = 1' | sudo tee /etc/sysctl.d/90-br-netfilter.conf
sudo modprobe br_netfilter
sudo sysctl --system
```

This sends traffic on every Linux bridge on the host through `iptables`, not only
Docker's. Docker's own rules still let containers on the same network talk, but Docker
also sets the `FORWARD` policy to `DROP`, so virtual machines or LXD containers on a
bridge of their own, such as `br0`, lose their network unless you allow that bridge, for
example `sudo iptables -I FORWARD -i br0 -o br0 -j ACCEPT`. Skip this on a host that runs
bridged virtual machines unless you know their rules.
<!-- UNVERIFIED: that loading br_netfilter changes nothing for the panel's other networks and other Compose projects, and that the game-to-game probe then gives 124. -->

### DNS

A game server needs DNS: crossplay, Steam and many mods look up names. Inside a
container on `valmin-games`, `/etc/resolv.conf` points at Docker's embedded resolver,
`127.0.0.11`. Docker answers container names itself and forwards every other query to
the host's resolvers. When the host runs systemd-resolved, Docker takes them from
`/run/systemd/resolve/resolv.conf` rather than `127.0.0.53`.

Whether the rules touch those queries depends on where Docker sends them from. Read the
`ExtServers` line from a container on the game network, using the game image from
`VALMIN_GAME_IMAGE` in `.env`:

```sh
docker run --rm --network valmin-games --entrypoint cat <game image> /etc/resolv.conf
```

```text
# ExtServers: [host(192.168.1.1)]
```

- An entry written as `host(…)` is queried from the host itself, not from the game
  server, so the rules do not see it. Leave `DNS` empty. Docker 29 marks a LAN router
  this way.
- An entry without `host(` is queried from inside the game server's network, so the query
  passes through `DOCKER-USER`. If it is a private address, such as the router, list it in
  `DNS`, for example `DNS="192.168.1.1"`, and run `valmin-isolate start` again. A listed
  resolver is then also reachable by a mod on port 53.
  <!-- UNVERIFIED: which Docker versions write entries without host(...), and that such queries carry the container's address. -->

A resolver on the host itself, such as the gateway `.15.1` or the host's LAN IP running
dnsmasq, is listed the same way. Either way, `getent hosts` in the check below proves that
names still resolve. If the host resolves through a resolver Docker cannot reach, see
[containers cannot reach Steam or Thunderstore](troubleshooting.md#containers-cannot-reach-steam-or-thunderstore);
pointing Docker's `dns` setting at your router is the case that needs a `DNS` entry here.

### IPv6

Valmin's Compose file does not turn on IPv6 for any of its networks, so game servers
have no IPv6 route out and the IPv4 rules are the whole job. If you add `enable_ipv6`
to `valmin-games`, repeat both chains with `ip6tables`: use the network's IPv6 subnet for
`S`, the panel's IPv6 address for `PANEL`, and `fc00::/7,fe80::/10` together with that
subnet for `PRIVATE`.

### Keep the rules after a reboot

The rules live in memory and are gone after a reboot. Install a systemd unit that applies
them whenever Docker starts, as `/etc/systemd/system/valmin-isolate.service`:

```ini
[Unit]
Description=Keep Valmin game servers off the LAN
After=docker.service
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/valmin-isolate start

[Install]
WantedBy=docker.service
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now valmin-isolate
```

`PartOf` reruns the script each time Docker restarts. The unit has no stop command, so
the rules stay in place while Docker restarts rather than lifting for a moment; the
script empties its chains before filling them, so running it again changes nothing.
After a reboot, Docker starts the game servers before this unit runs, so they run
unfiltered for the few seconds in between.
Saving the whole rule set with `iptables-persistent` instead would also save Docker's own
rules, which Docker rewrites on every start.

### ufw and firewalld

- **ufw.** ufw does not touch `DOCKER-USER`, so the unit covers game servers' connections
  through the host. For connections to the host itself, add them to ufw so its own rules
  keep them, and delete the `VALMIN-GAMES-IN` block from the script:

  ```sh
  sudo ufw insert 1 deny from 10.89.15.0/24
  sudo ufw insert 1 allow from 10.89.15.254
  ```

  Use your own prefix. The second rule goes above the first, so the panel stays allowed.
  <!-- UNVERIFIED: whether ufw reload or ufw enable removes a jump added to INPUT by hand. -->
- **firewalld.** With firewalld's `iptables` backend, `firewall-cmd --reload` clears rules
  it does not manage, and Docker then recreates an empty `DOCKER-USER`. Run
  `sudo systemctl restart valmin-isolate` after each reload, once Docker has recreated its
  chains. With firewalld's default `nftables` backend its rules live apart from these, and
  a reload leaves them alone.
  <!-- UNVERIFIED: firewalld reload behaviour with both of its backends. -->
- **Docker's nftables backend.** With `"firewall-backend": "nftables"` in
  `/etc/docker/daemon.json` there is no `DOCKER-USER` chain and these rules do not apply.
  <!-- UNVERIFIED: no DOCKER-USER chain with Docker's nftables firewall backend. -->

### Check it worked

Pick a running game server's container and probe a few addresses from inside it:

```sh
docker ps --filter label=io.valmin.managed=true --format '{{.Names}}'
C=<container name>
probe() { docker exec "$C" timeout 3 bash -c "</dev/tcp/$1/$2" 2>/dev/null; echo "$1:$2 exit $?"; }
```

Exit `0` means the connection opened and `1` that it was refused, so a packet arrived;
both mean the address is reachable. A wrong container name also gives `1`, so check the
name first. Exit `124` means no answer within three seconds,
which is what a dropped connection looks like.

| Probe | Expect |
| --- | --- |
| `probe <router IP> 80` | `124` |
| `probe <host LAN IP> 22` | `124` |
| `probe 10.89.15.1 22` (your prefix) | `124` |
| `probe 169.254.169.254 80` | `124` |
| `probe <another game server's IP> 2455` | `124` with [`br_netfilter`](#between-game-servers) loaded; otherwise `0`, or `1` without the console plugin |
| `probe 1.1.1.1 443` | `0` |
| `docker exec "$C" getent hosts steamcommunity.com` | An address |

Find another server's IP with
`docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' <container>`.

Then check that nothing Valmin does broke:

- Console commands still work on a running server.
- **Diagnostics → Run deep checks** still passes the game network check.
- A mod installs from the catalogue.
- A remote backup to a WebDAV server or NAS on your LAN still uploads, if you use one.
- A player on the LAN can join, and a crossplay server still logs `Game server connected`
  and reaches ready.
- After `sudo systemctl restart docker` and after a reboot, `sudo iptables -S VALMIN-GAMES`
  still lists the rules and the probes still give `124`.

### Remove the rules

```sh
sudo systemctl disable --now valmin-isolate
sudo /usr/local/sbin/valmin-isolate stop
sudo rm /etc/systemd/system/valmin-isolate.service /usr/local/sbin/valmin-isolate
sudo systemctl daemon-reload
```

If you added the ufw rules, delete them with `sudo ufw delete deny from <subnet>` and
`sudo ufw delete allow from <panel address>`.

If you loaded `br_netfilter` for [traffic between game servers](#between-game-servers),
remove `/etc/modules-load.d/br_netfilter.conf` and `/etc/sysctl.d/90-br-netfilter.conf`;
it unloads at the next reboot.
