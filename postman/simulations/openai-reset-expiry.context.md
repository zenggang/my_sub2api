# OpenAI reset-credit expiry

## Purpose

Run the real quota HTTP client and automatic expiry evaluator against a local Codex stand-in. All account identifiers, tokens and cards in the mock are synthetic. No production traffic is proxied into it, and no workspace resources are published.

## Assumptions and evidence

- Account setting and scan behavior are defined by this change. Local simulations verify the implementation rather than upstream entitlement rules.
- The `nothing_to_reset`, `windows_reset: 0`, `credit.status: available` response matches one authorized account-25 request on 2026-10-29. Before and after that request, the available count remained two and reported usage remained 14 percent. This observation does not guarantee all future reset requests preserve cards.
- The successful redemption, first-attempt 503 and unknown zero-window response cases are synthetic. They exercise targeting, stable retry identity, persisted outcomes and refresh behavior; they do not establish how often the real provider accepts a reset.
- The local CLI has no owning workspace binding for this private endpoint. This fixture follows repository contracts and the sanitized observation; no provider-owned mock was imported.

## Execution

```sh
postman simulation run postman/simulations/openai-reset-expiry.sim.yaml --port 4910 --no-history --output ndjson
cd backend
TEST_OPENAI_RESET_EXPIRY_SIMULATION_URL=http://127.0.0.1:4910/codex go test -tags=unit,simulation ./internal/service -run '^TestOpenAIResetExpiryPostmanSimulation$' -count=1
```

Start a fresh mock for each run to clear its synthetic state. Inspect simulation request events and the mock's `/_requests` endpoint to confirm the implementation reached the dependency. Stop the local simulation after verification.

## Results

2026-10-29: all four scenarios passed through the real expiry evaluator and quota HTTP client. The fixture request log confirmed that an account with disabled status, disabled scheduling and usage-based auto-reset switched off still reached the targeted consume endpoint through expiry evaluation. A successful redemption refreshed the available count to zero; `nothing_to_reset` retained one card and a recreated evaluator did not redeem again. A first-attempt 503 retried the exact same card and redemption UUID. An unknown zero-window response stayed unconfirmed and replayed without a second upstream consume request. The simulation's request events and `/_requests` observations both confirmed actual HTTP calls. These checks used synthetic repositories, cards and tokens; they do not represent a production release or a real successful card consumption.
