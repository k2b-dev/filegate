---
title: S3 client recipes
navTitle: S3 clients
section: Deep reference
order: 287
description: Known-good Filegate configuration for common S3-compatible clients.
tags: [reference, s3, clients]
---

# S3 Client Configuration

Tested config snippets for popular S3 clients pointing at a filegate listener.

All examples assume:

- Filegate exposed at `http://s3.example.com:9100` (plain HTTP behind a reverse proxy that does TLS to `https://s3.example.com`).
- One bucket named `data` configured in `storage.base_paths`.
- Example key `AKIAIOSFODNN7EXAMPLE` / secret `wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`.
- Region `us-east-1`.

Substitute your real values.

> **Path-style required.** Filegate only supports path-style addressing (`https://host/{bucket}/{key}`). Every client below has a flag for this — set it explicitly. Virtual-hosted-style (`{bucket}.host`) does **not** work.

---

## awscli

```bash
# ~/.aws/credentials
[filegate]
aws_access_key_id = AKIAIOSFODNN7EXAMPLE
aws_secret_access_key = wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
```

```bash
# ~/.aws/config
[profile filegate]
region = us-east-1
s3 =
    addressing_style = path
```

Usage:

```bash
aws --profile filegate --endpoint-url https://s3.example.com s3 ls s3://data/
aws --profile filegate --endpoint-url https://s3.example.com s3 cp file.txt s3://data/file.txt
aws --profile filegate --endpoint-url https://s3.example.com s3api list-objects-v2 --bucket data
```

For multipart uploads of large files, `aws s3 cp` automatically uses CreateMultipartUpload + UploadPart + Complete.

---

## rclone

```bash
# ~/.config/rclone/rclone.conf
[filegate]
type = s3
provider = Other
env_auth = false
access_key_id = AKIAIOSFODNN7EXAMPLE
secret_access_key = wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
endpoint = https://s3.example.com
region = us-east-1
force_path_style = true
no_check_bucket = true
```

`no_check_bucket = true` skips a HeadBucket probe rclone normally does on first contact — useful when your key isn't allowed to list at the root.

Usage:

```bash
rclone ls filegate:data
rclone copy ./local-dir filegate:data/remote-dir --progress
rclone sync filegate:data ./backup --transfers 4
```

---

## restic

restic stores its repository as plain S3 objects under a chosen prefix.

```bash
export AWS_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
export AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
export RESTIC_REPOSITORY="s3:https://s3.example.com/data/restic-backup"
export RESTIC_PASSWORD="your-restic-encryption-passphrase"

restic init     # creates the repo layout under data/restic-backup/
restic backup ~/Documents
restic snapshots
```

restic uses path-style automatically when given an explicit endpoint.

---

## kopia

```bash
kopia repository create s3 \
  --bucket=data \
  --endpoint=s3.example.com \
  --region=us-east-1 \
  --access-key=AKIAIOSFODNN7EXAMPLE \
  --secret-access-key=wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY \
  --prefix=kopia-backup/
```

Then connect with `kopia repository connect s3 …` using the same flags. kopia uses path-style addressing by default for custom endpoints.

---

## Bun.s3

```typescript
import { S3Client } from "bun";

const s3 = new S3Client({
  accessKeyId: "AKIAIOSFODNN7EXAMPLE",
  secretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
  region: "us-east-1",
  endpoint: "https://s3.example.com",
  bucket: "data",
});

// Upload
await s3.write("hello.txt", "hello, filegate");

// Read
const text = await s3.file("hello.txt").text();

// Large upload. Filegate accepts either PutObject or multipart,
// depending on the wire format Bun chooses for the payload.
await s3.write("big.bin", new Uint8Array(50_000_000));

// Stream copy
const copy = await s3.copy("hello.txt", "hello-copy.txt");
```

Bun.s3 sends path-style by default when given an explicit `endpoint`.

---

## MinIO Client (mc)

```bash
mc alias set filegate https://s3.example.com \
  AKIAIOSFODNN7EXAMPLE \
  wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY \
  --api S3v4 \
  --path on

mc ls filegate/data
mc cp file.txt filegate/data/file.txt
mc mirror ./local-dir filegate/data/remote-dir
```

`--path on` selects path-style addressing.

---

## Cyberduck

Cyberduck has a GUI for adding S3 connections.

1. **Open Connection → S3**.
2. **Server**: `s3.example.com`.
3. **Port**: `443` (HTTPS) or `9100` (plain HTTP, only for local testing).
4. **Access Key ID**: `AKIAIOSFODNN7EXAMPLE`.
5. **Secret Access Key**: `wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY`.
6. **More Options → Path**: leave blank to land at the bucket-list root, or set `/data` to land directly inside a specific bucket.
7. **More Options → Use legacy SSL**: leave off (modern TLS is fine).
8. **More Options**: under **Connection mode**, ensure **Use virtual host style** is **OFF**. Cyberduck's flag may be labeled "Path-Style requests" — turn it on.

In the connection profile XML (advanced users):

```xml
<key>S3 Path Style</key>
<string>true</string>
<key>S3 Bucket Virtual Host</key>
<string>false</string>
```

After connecting, you'll see your authorized buckets listed at the top level. Drag and drop works for upload/download. Multipart and CopyObject are used transparently.

### Cyberduck quirks

- Cyberduck issues some bucket-level probe ops (HeadBucket, GetBucketLocation) that we either implement or politely 200/403 on. The browser pane will populate normally as long as the key is authorized for the buckets it's listing.
- On rename / move within the same bucket, Cyberduck uses CopyObject + DeleteObject — fast on reflink-capable filesystems and correct under filegate's atomic-rename semantics.

---

## s3cmd

```ini
# ~/.s3cfg
[default]
access_key = AKIAIOSFODNN7EXAMPLE
secret_key = wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY
host_base = s3.example.com
host_bucket = s3.example.com/%(bucket)s
use_https = True
signature_v2 = False
```

Note the `host_bucket = host_base/%(bucket)s` shape — this forces path-style.

```bash
s3cmd ls s3://data/
s3cmd put file.txt s3://data/file.txt
```

---

## Local-only quick test

For sanity-checking a fresh deployment **without** a reverse proxy:

```bash
# Bypass HTTPS — plain-HTTP for local tests.
aws --endpoint-url http://localhost:9100 \
    --no-verify-ssl \
    s3 ls s3://data/

curl -v -X HEAD http://localhost:9100/data/some-key \
  -H "Authorization: AWS4-HMAC-SHA256 …"
```

`curl` won't sign for you — for end-to-end testing prefer `awscli` or `rclone`. To verify the listener is up at all:

```bash
curl http://localhost:9100/   # → 403 SignatureDoesNotMatch (SigV4 required)
```

A 403 here is the **expected** response — it means the listener is alive and rejecting unsigned requests, which is what we want.

---

## See also

- [S3 API compatibility](/docs/en/reference/s3-api) — supported operations and deviations.
- [S3 server configuration](/docs/en/reference/s3-config) — server-side config and auth model.
