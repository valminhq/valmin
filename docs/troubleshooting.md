# Troubleshooting

[Documentation](README.md) / Troubleshooting

Run these commands from `deploy/`:

```sh
docker compose ps
docker compose logs --tail=100 valmind caddy docker-proxy
docker compose exec valmind valmind version
docker compose exec valmind valmind healthcheck
```

| Symptom                                                              | Check                                                                                                                                               |
| -------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| Setup token is rejected                                              | Use the newest token. Restart `valmind` to generate another if no administrator exists yet.                                                         |
| `host_data_root self-check` fails                                    | Check the host path and bind mount. Confirm the game image exists locally.                                                                          |
| Data directory is not writable or provisioning reports the wrong UID | Check ownership is `10000:10000` and the panel runs as UID 10000.                                                                                   |
| `No such image` during provisioning                                  | Run `prepare-host.sh` again with the configured data path to obtain the game and SteamCMD images.                                                   |
| Requests return `403 origin_rejected`                                | Match `VALMIN_SERVER_EXTERNAL_URL` to the exact browser address. With another proxy, preserve WebSocket upgrades and configure its trusted address. |
| Server runs but players cannot connect                               | Check job and game logs, assigned UDP ports, NAT rules, password, and client mod versions.                                                          |
| Console has no command input                                         | This build supports log viewing only.                                                                                                               |

To reset an existing account password:

```sh
docker compose exec valmind valmind admin reset --username YOUR_USERNAME
```

The command prints a new password and revokes that user's sessions.
