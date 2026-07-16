# S3 integration tests

These tests exercise squirrel's direct S3 read path (`sync/s3reader.go`)
against a real S3-compatible endpoint instead of the in-process fake used by
the unit tests. They answer the one question the fake cannot: does the server
return a **composite multipart ETag** (`<hex>-<parts>`) that `s3reader.go`
depends on, and does a multipart object round-trip byte-for-byte?

The reference endpoint is [SeaweedFS](https://github.com/seaweedfs/seaweedfs)
(Apache-2.0, single Go binary). Any S3-compatible server works — point the
environment variables at it.

## Running

The tests are gated behind the `integration` build tag **and** skip unless
`SQUIRREL_TEST_S3_ENDPOINT` is set, so `go test ./...` stays hermetic.

```sh
docker compose -f test/integration/docker-compose.yml up -d

# Wait for the S3 gateway to start serving (a bare GET returns 403 once up).
until curl -s -o /dev/null http://127.0.0.1:8333; do sleep 1; done

export SQUIRREL_TEST_S3_ENDPOINT=http://127.0.0.1:8333
export SQUIRREL_TEST_S3_ACCESS_KEY=squirreltest
export SQUIRREL_TEST_S3_SECRET_KEY=squirreltestsecret

go test -tags integration ./sync/ -run TestS3 -v

docker compose -f test/integration/docker-compose.yml down -v
```

> The fixture has no container healthcheck (the 3.80 image ships neither
> `curl` nor a `/status` route on the S3 API), so readiness is polled from
> the host against the published port instead of via `--wait`.

## Environment variables

| Variable                       | Required | Default                | Meaning                          |
| ------------------------------ | -------- | ---------------------- | -------------------------------- |
| `SQUIRREL_TEST_S3_ENDPOINT`    | yes      | —                      | S3 endpoint URL; unset ⇒ skip    |
| `SQUIRREL_TEST_S3_ACCESS_KEY`  | no       | (unauthenticated)      | access key id                    |
| `SQUIRREL_TEST_S3_SECRET_KEY`  | no       | (unauthenticated)      | secret access key                |
| `SQUIRREL_TEST_S3_BUCKET`      | no       | `squirrel-integration` | bucket to create and use         |
| `SQUIRREL_TEST_S3_REGION`      | no       | (empty)                | region passed to the client      |

## What the tests assert

- **`TestS3MultipartCompositeETag`** — uploads an 11 MiB object as a 3-part
  multipart upload and asserts the ETag read back through the production
  `s3ETagReader` matches `^[0-9a-f]{32}-[0-9]+$` with >1 part, then verifies
  the object downloads byte-for-byte. If this fails on a given server, that
  server cannot back squirrel's S3 fingerprint path.
- **`TestS3SinglePartPlainETag`** — uploads a 1 MiB single-part object and
  asserts its ETag is a bare 32-hex MD5 (no `-`), pinning the contrast the
  reader relies on.
