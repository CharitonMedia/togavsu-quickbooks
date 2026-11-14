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
go vet ./...
go test ./...
docker build -t togavsu-quickbooks .
```

The acceptance tests (`TestMODQBO01Install` through `TestMODQBO34TunableSettings`) run
against an in-memory QuickBooks (`qbo/fake`), a fake core (`internal/fakecore`) and a
Postgres database named in `TOGAVSU_TEST_DATABASE_URL`; they skip when it is unset outside
CI. Use a database of your own, such as `togavsu_quickbooks`: each test drops and
recreates its `core` and `mod_quickbooks` schemas. No test reaches Intuit.

## Signing

`quickbooks package -key signing.key -out quickbooks.signed.json` builds the signed
package through the `Packager` interface in `package.go`. The archive and signature formats
belong to the core, so the module does not copy them: until the core publishes a public
signing package or CLI, the stub packager answers that packaging is unavailable.

## License

Source-available under the same license as Togavsu. See [LICENSE](LICENSE).
