# Wings sync notes

This integration merges `pterodactyl/wings` develop through
`d6116827313dae176ddf4741e233554392993398` into CatWings based on `2eec746`.
The histories diverged, so this is a merge rather than a Git fast-forward.

## CatWings compatibility

- Keep ZIP backup creation, restoration, downloads, and file compression; keep
  tar/gzip server transfers and the ZIP-entry endpoint.
- Apply upstream backup UUID validation to ZIP paths and the CatWings base
  backup methods. Keep size/checksum validation read-only because backup details
  calculates them concurrently. Adapt restore MIME validation and its tests to ZIP.
- Keep local filesystem paths protected during Panel configuration updates,
  alongside upstream credential rotation and remote-client credential updates.
- Keep CatWings branding, memory reporting, and existing mount configuration.
- Restrict the new upstream CDN release dispatch to `pterodactyl/wings`.

## Deferred upstream requirement

JWT scope enforcement from `d0ddc80844479302abdaf9654de3bacd511c0f5c` is
deferred. The adjacent Panel checkout's `NodeJWTService`, websocket controller,
file controller, and backup download service issue tokens without `scope`.
Requiring it would reject existing Panel requests. Keep existing authorization
and the new token revocation checks, plus upstream token redaction in logs.

Enable scope enforcement in a coordinated Panel/daemon update once websocket,
file-upload, file-download, backup-download, and transfer tokens all carry their
corresponding scope. This sync therefore does not include that upstream security
improvement.

## Deployment considerations

Upstream now blocks private/internal backup restore destinations unless allowed
by `system.backups.restore_host_allowlist`. Private S3-compatible storage needs
an explicit hostname, IP, or CIDR entry before rollout.

Upstream also adds CPU period, burst, and shares settings. Burst defaults to
enabled on supported kernels; CPU shares now defaults to the Docker engine
default. These settings can affect scheduling under load.

Validation includes Go tests, race tests, Linux amd64/arm64 builds, and targeted
regressions for ZIP backup round trips, unscoped Panel backup downloads and
one-time token reuse rejection, and configuration path protection during token
rotation. Automated checks do not replace a staging smoke test of console,
SFTP, local/S3 backup restore, server transfer, and container resource updates.
