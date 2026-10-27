# Usage push recovery simulation

The local mock implements this repository's Keeper push v1 contract. It returns one 503 followed by successful acknowledgements. Owner discovery was unnecessary: both ends are developed in this task, and no published owner mock exists for the new interface.

Start from the repository root with `postman simulation run test/postman/simulations/usage-push.sim.yaml --port 4907 --no-history --output ndjson`. Restart it before each recovery run; its request counter is process-local.

From the repository root run `KEEPER_PUSH_SIMULATION_ENDPOINT=http://127.0.0.1:4907/keeper/api/v1/ingest/usage go test ./push -run TestPostmanSimulationTransport -count=1 -v`.

On 2026-10-26 this test passed against Postman CLI 1.73.0. The simulation logged two real POST requests, first 503 and then 200, carrying the same delivery ID. The plugin retained the first failed upload and deleted its queue entry only after acknowledgement. All simulation members served traffic. No real CPA credentials or central server were used; this proves HTTP retry handling against the saved stand-in, independently of the real Keeper integration tests.

CLI details established during this run: use a `version: 1` mock manifest and point the simulation at its `config.yaml`, rather than the folder. No files were published to a workspace. Request logs are temporary and are not committed.
