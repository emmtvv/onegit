# Container registry

onegit is also an OCI/Docker registry on the same port. Images are named
`<host of ONEGIT_BASE_URL>/<name>`; any lowercase name works (`app`,
`team/api`, `a/b/c`).

```sh
docker login git.example.com           # password, or an access token (required for SSO users)
docker push git.example.com/team/api:1.0
docker pull git.example.com/team/api:1.0
```

Docker only talks plain HTTP to `localhost`; for any other host put onegit
behind TLS or add the host to Docker's `insecure-registries`.

## Access

Access follows the repository roles, like Gitea's owner-level package
access: readers pull, writers push and delete. With `ONEGIT_PUBLIC_READ`
anonymous clients may pull. Clients authenticate with Basic auth (password or
access token) or exchange it at `/v2/token` for a 15-minute bearer token.
Tokens carry no scopes: every request is checked against the user's current
role, so revoking access takes effect at once.

CI jobs push with `$ONEGIT_TOKEN`. The registry records which job built an
image, from which commit and ref, and a tag built by CI cannot be moved by a
person (CI can move it; an administrator can delete it first). Deployment
rules use this provenance ([deployments.md](deployments.md)).

## Web UI

**Packages** lists images and versions (tags and untagged manifests, newest
first, searchable). A version page shows pull commands by tag and by digest,
the platforms of a multi-platform image, the layers with the Dockerfile
command that created each, labels and the image configuration. Writers can
delete a version or a whole image.

## Storage and cleanup

Layers live in S3, deduplicated across all images; manifests live in
Postgres. Chunked uploads may hit different replicas.

- **Cleanup rule** (Admin → Packages, with a preview and "run now"), with
  Gitea's semantics: per image, versions newest first; keep the first N;
  `latest` is always kept; keep versions matching the keep pattern or
  younger than N days; with a remove pattern, remove only matching versions.
  Patterns are case-insensitive regular expressions matching the whole
  version (or `image/version` with "match full name"). The rule runs every
  6 hours.
- **Garbage collection** removes layers no manifest references after a
  24-hour grace period, every 6 hours. Run it now with
  `onegit admin registry-gc [-grace 1h]`.
- **Limits**: `ONEGIT_REGISTRY_MAX_BLOB_SIZE` per layer and
  `ONEGIT_REGISTRY_MAX_TOTAL_SIZE` for the whole registry.

## API

The distribution API is at `/v2/`: blobs (monolithic and chunked uploads,
cross-repository mounts, `Range`), manifests (OCI and Docker v2 images and
indexes, up to 4 MiB), tags (paginated), catalog and deletes
(`DELETE /v2/<name>/manifests/<digest>` removes a version,
`.../manifests/<tag>` only the tag).

A Gitea-compatible packages API keeps existing scripts working. A Gitea
image `host/<owner>/<name>` is the onegit image `<owner>/<name>`:

```
GET    /api/v1/packages/{owner}?type=container&q=&page=&limit=
GET    /api/v1/packages/{owner}/container/{name}
GET    /api/v1/packages/{owner}/container/{name}/{version}
DELETE /api/v1/packages/{owner}/container/{name}/{version}
GET    /api/v1/packages/{owner}/container/{name}/{version}/files
```

Authenticate with `Authorization: token <access token>`.
