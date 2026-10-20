# Grizzly Jio integration

Approved in chat on 2026-10-15.

## Scope

Port the local `D:/code/demo/jio-gemini-otp/jio.py` flow to a Go JioProvider named `grizzly`, displayed as Grizzly. Each purchase owns an isolated Jio cookie session. Keep existing proxy settings. Do not copy credentials or generated links from the script project.

Grizzly inventory is explicitly unlimited. Procurement still requires valid configuration, a positive USD price and adequate balance. Configure API key, maximum number price, total timeout and maximum number attempts. Default service is MyJio (`jio`), country India (`22`). Retry missing stock, rejected numbers and missing OTP within bounded time; cancel unused activations and finish successful ones. Use actual activation cost when available. Failed attempts may incur upstream charges; cancellation is best effort.

All suppliers have persisted enable/disable state, default enabled for existing suppliers and disabled for new Grizzly until configured. Disabled suppliers cannot be selected automatically, selected manually or used for new manual purchases. Existing running purchases continue. Invalidate quote caches on configuration/state changes and recheck state at dispatch.

## Redemption

Preserve the synchronous API for existing consumers. The C-end opts into asynchronous redemption. Validate and atomically reserve the card using the existing handler before starting a background worker. Persist a random task token, percentage and final response in MySQL. Poll by POST with the task token (never put card secrets in URLs). Refresh resumes polling from session storage. Duplicate submissions reuse the active task and never initiate another purchase.

Grizzly stages drive estimated percentages: number acquisition 2–19 (about 15 seconds), check/send 20–26 (about 5 seconds), OTP 27–94 (about 55 seconds), verify/extract 95–99. Progress is monotonic, bounded below 100 until the existing link persistence and settlement finish. C-end displays only a bar and percentage. No business-stage descriptions. The task survives browser disconnects. Expired/interrupted tasks report a failure requiring review, without automatically unlocking a potentially charged card.

## Validation and implementation order

1. Supplier switch persistence, disabled-selection guards and cache invalidation.
2. Grizzly adapter: balances, unlimited catalog, bounded acquisition, isolated cookies, OTP, URL extraction and cancellation.
3. Durable async tasks around the existing card lock and settlement, percentage callbacks and refresh resume.
4. Admin fields/switches and accessible C-end progress in the current visual style.
5. Local HTTP fixture tests for successful and failing Grizzly flows, cancellation, costs and timeout; database tests for selection, async idempotency and settlement; JS syntax/interaction and build checks. Never use real paid activations during automated checks.
