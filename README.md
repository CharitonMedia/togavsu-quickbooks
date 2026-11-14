# QuickBooks Sync for Togavsu

A Togavsu module that pushes the firm's money records into QuickBooks Online, one way.
Togavsu stays the system of record: the module never writes a Togavsu money record, and
nothing flows back from QuickBooks.

What reaches QuickBooks:

- Billed contacts become Customers (matched to an existing customer by exact name when its
  notes name no other Togavsu contact; a name a vendor holds gets the contact id suffix).
- Sent, viewed and paid invoices become Invoices, line for line, with `EmailStatus NotSet`
  and online payment off, so QuickBooks never contacts a client. A write-off is a
  CreditMemo plus a zero Payment that applies it.
- Completed operating payments become Payments, deposited to Undeposited Funds or the
  operating bank (`payment_deposit`), applied to the synced invoice. Reversals void them.
- Every trust ledger entry becomes a JournalEntry between the trust bank and the client
  trust liability, so a deposit is never income. A transfer to operating that pays an
  invoice posts two lines plus the linked Payment; one that pays no invoice posts four
  lines to the earned-from-trust income account. A reversal mirrors its original.
- In purchases mode, expenses become Purchases to the client costs account.

Records on a walled matter never reach the module: it reads through the core's books
reader, which honors walls, and the core sends it no event for them.

The full specification is [docs/SPEC.md](docs/SPEC.md).

## Setup

1. Install the module and approve its ten scopes (books:read is required).
2. In the module settings, enter the Intuit app's client ID and the account mapping
   (an account or item Id, or its exact full name). Every tunable is a setting:
   `retry_schedule` (default `1m,5m,30m,2h,6h`), `alert_interval_minutes` (15) and
   `reconcile_hour_utc` (8).
3. On the QuickBooks setup tab, register the redirect URI it shows in the Intuit app,
   enter the client secret (the core's secret store keeps it, with the QuickBooks tokens;
   no module table holds them), then press Connect, which opens its own window.
4. Create missing accounts and items makes the default trust liability, income, client
   costs accounts and items the company lacks. It never makes a bank account.

`qbo_environment` defaults to sandbox, and every page shows a Sandbox banner until it is
set to production.

## Screens and agent endpoints

The widget, the books tab, the setup tab and the matter tab each have a JSON twin, and
every action is a form post from the frame and a JSON endpoint. All of them need the
viewer's context token in `X-Togavsu-Context-Token`. The OpenAPI description is at
`/m/quickbooks/api/openapi.json`.

| Endpoint | Who | What |
| --- | --- | --- |
| GET /m/quickbooks/api/status | admin, accountant | synced, pending and exception counts |
| GET /m/quickbooks/api/books | admin, accountant | exceptions (`status=`) and the books view (`month=YYYY-MM`) |
| GET /m/quickbooks/api/setup | admin, accountant | connection, mapping checks, chart of accounts, trust bank map |
| GET /m/quickbooks/api/matter | firm staff | one matter's records with their QuickBooks state |
| POST /m/quickbooks/api/retry | admin, accountant | retry a failed row (`kind`, `id`) |
| POST /m/quickbooks/api/repush | admin, accountant | push Togavsu's version over a drift row |
| POST /m/quickbooks/api/acknowledge | admin, accountant | keep a drift row quiet until the record changes |
| POST /m/quickbooks/api/sync-now | admin, accountant | run the reconcile now |
| POST /m/quickbooks/api/disconnect | admin | revoke the sign-in; sync rows stay |
| POST /m/quickbooks/api/create-missing | admin | create the default accounts and items |
| POST /m/quickbooks/api/trust-bank-map | admin | map a trust account to a QuickBooks bank |
| POST /m/quickbooks/api/client-secret | admin | store the Intuit client secret |
| POST /m/quickbooks/api/reset | admin | clear the sync tables (`confirm=RESET`); the log stays |

## Build

```
gofmt -l .
go vet ./...
go test -race ./...
docker build -t togavsu-quickbooks .
```

The module builds on the public module kit, `pkg/modulekit` in the core, pinned in
`go.mod`. Two suites cover `MOD-QBO-01` through `MOD-QBO-34`, both against an in-memory
QuickBooks (`qbo/fake`) and Postgres named in `TOGAVSU_TEST_DATABASE_URL`. Both skip when
it is unset outside CI and fail in CI without it. No test reaches Intuit.

- The root package runs every criterion on a fake core (`internal/fakecore`). Each test
  drops and recreates the `core` and `mod_quickbooks` schemas in that database, so use a
  database of your own, such as `togavsu_quickbooks`.
- `acceptance` runs the same criteria on the composed core, `pkg/modulekit/coretest`. It
  signs the package with a throwaway key, trusts the key, installs and approves it, and
  serves the module in process. coretest creates and drops its own databases and roles, so
  the URL must be a superuser's. A criterion that needs a core item the pinned core lacks
  skips naming that `CORE-NN` item (see `docs/SPEC.md`, "Where this module builds"), and
  passes once the core lands it and the pin moves.

## Packaging and signing

The package keeps every part it needs, including the ones that wait on a module-core
change. `package/pending.yaml` lists each of those parts once, with the `CORE-NN` item it
waits on and what it does; the kit's `modulekit.Installable` holds back each listed part
the pinned core refuses and keeps each one it accepts, so a part comes back on its own
when its item lands and the pin moves. At the pinned core it holds back:

| Part | Waits on |
| --- | --- |
| manifest `notifications` (the sync problem notification) | CORE-03 |
| manifest `sidebar[*].section`, `icon`, `order` | CORE-08 |
| scope `books:read` | CORE-02 |
| scope `module_secrets:write` | CORE-06 |
| `migrations/0004_trust_bank_fk.sql` (the trust bank map's link to `core.trust_accounts`) | CORE-22 |

A held-back migration is a whole file numbered after every migration that installs, so it
is added later as a new migration and no migration a firm has run ever changes. Until
0004 installs, the trust-bank-map action checks the trust account against the core's list
itself, so no row can stop 0004 from adding the link.

```sh
quickbooks package -key signing.key -out quickbooks.signed.json
# or, from the package directory alone:
go run github.com/CharitonMedia/togavsu/cmd/togavsu-module package -installable -dir package -key signing.key -out quickbooks.signed.json
```

Both sign the installable package (`modulekit.BuildInstallable`), print one stderr line per
held-back part with its CORE item, warn about any pending entry that holds nothing back,
and print the public key the admin adds as a trusted signing key. `signing.key` holds the
base64 Ed25519 seed the kit's `ParseSeed` reads; keep the release key out of the
repository. Either refuses to sign when the core refuses a part `pending.yaml` doesn't
list. `quickbooks.Archive` gives the installable tar. The manifest's container image digest
is a placeholder that the release workflow pins.

`TestPackageSignsAndVerifies` builds and signs with a key made at test time, verifies the
signed file as the core does at install and checks its files have no refusals. It fails
when the build fails with a valid key (a refusal `pending.yaml` misses), when a pending
entry holds nothing back (drop it), and when a part is held back for anything but a field,
scope or core table the core doesn't know yet. CI also runs
`togavsu-module check -installable package`.

## License

Source-available under the same license as Togavsu. See [LICENSE](LICENSE).
