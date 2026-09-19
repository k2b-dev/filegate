# DirectSession browser contracts

Build the SDK and start the local fixture from `sdk/ts`:

```sh
bun run build
bun test/browser/serve.ts
```

Open the printed `page` URL in a browser. The result appears on the page and in
`globalThis.browserContractResult`. Restart the fixture before another run;
its attempt counters belong to one run. Stop it with Ctrl-C.

The page imports the current built `dist/utils.js`. Two ephemeral loopback ports
exercise native browser fetch, real CORS preflights, Blob retries, Retry-After,
bounded attempts, cancellation, and absence of backend credentials. It needs no
browser test dependency. A same-origin cookie control verifies that DirectSession,
putDirect, archiveRaw, and Filegate.downloadRaw omit browser cookies even when the
browser would normally send them. The second server simulates transfer responses; this
checks SDK/browser behavior, not Filegate server authorization. Real server
contracts are covered separately by Linux HTTP integration tests.
