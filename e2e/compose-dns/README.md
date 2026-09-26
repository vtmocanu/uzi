# Compose DNS regression

Run `./e2e/compose-dns/run.sh` with Docker Compose v2 and curl installed.

This standalone check starts the shipped nginx config with a tiny API fixture.
It verifies REST and WebSocket routing, removes the API container, occupies its
former address, and starts a replacement API without restarting web. Both routes
must then identify the new API container. The script uses an isolated Compose
project, a random loopback host port, and no application database or credentials.
It tears down only its own project on exit.
