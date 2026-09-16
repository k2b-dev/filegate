# @k2b/filegate

Root-scoped TypeScript client for Filegate. Use it on trusted backends; give
browsers scoped direct URLs and the token-free `@k2b/filegate/utils` helpers.

```ts
import { Filegate } from "@k2b/filegate";
const files = new Filegate({ baseUrl: "https://files.example.org", token });
const upload = await files.root("documents").directUpload("notes.txt", 5);
// Authorized browser: fetch(upload.url, { method: "PUT", body: "hello" })
```

See the [TypeScript guide](https://filegate.dev/docs/en/ts-sdk),
[transfer guide](https://filegate.dev/docs/en/uploads-downloads) and
[portable skill](https://github.com/k2b-dev/filegate/tree/main/skills/filegate).
