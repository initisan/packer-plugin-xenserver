# XenServer Packer.io builder

> **This repository is a read-only release mirror.**
> Development and review happen in an internal repository; every commit on
> `main` here is a snapshot copied from it, and the published releases are built
> and signed there. Pull requests are not accepted, and any change committed
> directly to this repository is overwritten by the next snapshot. Please open an
> issue instead.

This builder plugin extends packer.io to support building images for XenServer.

It contains 2 different builders:
- `xenserver-vm-iso` -> With the ISO builder you can create a VM from scratch by auto-installing the OS.
- `xenserver-vm-clone` -> With the Clone builder you can clone an existing Template / VM for further customization.

Both builders can export the finished VM as an XVA; `xva` remains a supported
export *format* (see `format`), it is simply no longer a builder.

## Status

At the time of this writing the packer builder has been verified to work with XenServer 9.

## Using the builder

The packer builder can be installed via `packer init` as long as the packer template includes the following in its `pkr.hcl` file
```
packer {
  required_plugins {
   xenserver= {
      version = ">= 1.0.0"
      source = "github.com/xenserver/xenserver"
    }
  }
}
```

The following command will install the packer plugin using the Ubuntu example provided in this repository.

```
packer init examples/ubuntu-2004/ubuntu-2004.pkr.hcl
```

If you cannot use `packer init`, download the archive for your platform from the
[releases page](https://github.com/xenserver/packer-plugin-xenserver/releases) and
install it by hand:

```shell
$ unzip packer-plugin-xenserver_v1.0.0_x5.0_linux_amd64.zip
$ packer plugins install --path packer-plugin-xenserver_v1.0.0_x5.0_linux_amd64 \
    github.com/xenserver/xenserver
$ packer plugins installed
```

## Verifying a release

Every release ships a SHA-256 checksum file and a detached OpenPGP signature
over it, made with the XenServer Packer plugin signing key:

```
packer-plugin-xenserver_v<version>_SHA256SUMS
packer-plugin-xenserver_v<version>_SHA256SUMS.sig
```

Packer does not check that signature. `packer init` and `packer plugins install`
verify the SHA-256 checksums and nothing else, so the steps below are a
deliberate extra check rather than something the tooling does for you.

The public key is in [`KEYS`](KEYS) in this repository, and is attached to every
release.

1. Import the key and confirm its fingerprint:

   ```shell
   $ gpg --import KEYS
   $ gpg --fingerprint security@xenserver.com
   ```

   The fingerprint must be:

   ```
   FFC5 D9E4 BA7A F786 2C48  3480 4489 5E74 8988 4299
   ```

   Record that fingerprint the first time you verify a release. The key ships
   alongside the releases it signs, so reading the fingerprint only from here
   does not by itself establish that a release is genuine — anyone able to
   replace the artifacts could change this page too. What it does establish is
   that every release is signed by the same key, so a later change of signing
   key is visible to you.

2. Verify the signature over the checksum file:

   ```shell
   $ gpg --verify packer-plugin-xenserver_v<version>_SHA256SUMS.sig \
                  packer-plugin-xenserver_v<version>_SHA256SUMS
   ```

   This must report a good signature from the key above.

3. Verify the artifacts against the checksums:

   ```shell
   $ shasum -a 256 --check packer-plugin-xenserver_v<version>_SHA256SUMS
   ```

   Every file you downloaded must report `OK`.

## Developing the builder

### Dependencies
* Packer >= v1.7.0 (https://packer.io)
* XenServer 9
* Golang 1.26

## Compile the plugin

Once you have installed Packer, you must compile this plugin and install the
resulting binary.

Building also needs the XenServer Go SDK, which this repository does not carry:
`go.mod` has `replace xenapi => ./goSDK` and `goSDK/` is gitignored, so `make`,
`make dev` and `go build` all fail until the SDK's `sdk_go/src/*` contents have
been extracted into `./goSDK`. To fetch it:

1. Download the Go SDK from the matching
   [xapi-project/xen-api release](https://github.com/xapi-project/xen-api/releases)
   (`Go-SDK-*.zip`).

2. Extract its `sdk_go/src/*` contents into a `goSDK/` directory beside `go.mod`:

   ```sh
   unzip Go-SDK-*.zip -d sdk_temp
   mkdir -p goSDK
   mv sdk_temp/sdk_go/src/* goSDK/
   rm -rf sdk_temp
   ```

Then build with the Go version pinned in `go.mod`:

```shell
# Just build the Plugin
$ make
# Build the plugin and move it to ~/.packer.d/plugins/
$ make dev

#Manual steps:
$ go build -o packer-plugin-xenserver

# Add the plugin to the location packer expects it to be installed in
$ mkdir -p ~/.packer.d/plugins/
$ cp packer-plugin-xenserver ~/.packer.d/plugins/packer-plugin-xenserver
```

## Documentation

For complete documentation on configuration options, see the
[xenserver-vm-iso](docs/builders/xenserver-vm-iso.mdx) and
[xenserver-vm-clone](docs/builders/xenserver-vm-clone.mdx) builder docs

## Documentation: generated vs hand-written

Some of the Markdown in this repository is **build output** and is overwritten by
`make generate`. Editing those files by hand looks like it works and is silently lost on
the next run, so check this table before changing anything under `docs/`,
`docs-partials/` or `.web-docs/`.

| Path | Edit by hand? | Produced by |
| ---- | ------------- | ----------- |
| `docs/**.mdx`, `docs/README.md` | **Yes** — these are the sources | — |
| `docs-partials/xenserver/**.mdx` | **Yes** — shared prose, pulled in with `@include` | — |
| `docs-partials/builder/**.mdx` | **No** | `go generate ./...` → `packer-sdc struct-markdown`, from the Go struct field comments |
| `.web-docs/**/README.md` | **No** | `make generate` → `packer-sdc renderdocs` → `compile-to-webdocs.sh` |
| `.web-docs/metadata.hcl`, `.web-docs/scripts/**` | **Yes** — these drive the generation | — |

Two things about that layout are easy to trip over. `docs-partials/` holds both
hand-written and generated partials, told apart only by whether the path starts with
`builder/`. And `.web-docs/` is named like an output directory but also contains the
metadata and script that produce the output.

To change what a published page says, edit the right **source** and regenerate:

- Option tables in a builder page → edit the option list itself: almost all of them are
  written inline in `docs/builders/*.mdx`. The exceptions are the shared blocks each page
  `@include`s — connection, host SSH access, guest tools and build provenance, from the
  hand-written `docs-partials/xenserver/common/*.mdx`, plus the blocks that come from the
  Packer SDK's own partials and so cannot be edited in this repository: the SSH / WinRM
  communicator tables on both pages, and the ISO-download tables on the iso page
  (`packer-plugin-sdk/multistep/commonsteps/ISOConfig-required.mdx` and
  `ISOConfig-not-required.mdx`). The `mapstructure` field comments on `CommonConfig`
  in `builder/xenserver/common/common_config.go` and on `ISOConfig` / `CloneConfig` in
  `builder/xenserver/{iso,clone}/config.go` only feed `docs-partials/builder/**`, which
  `make generate` keeps current but which no page `@include`s yet, so changing a field
  comment on its own changes nothing a reader sees. Keep the two in sync, then run
  `make generate`.
- Prose in a builder page → edit `docs/builders/*.mdx` (or the shared partial it
  `@include`s), then `make generate` to refresh `.web-docs/`.
- "Upgrading from the earlier XenServer plugin" → edit
  `docs-partials/xenserver/common/Upgrade*.mdx`. That section appears on two pages which
  reach their readers by different routes, so it is held in partials rather than written
  twice: `docs/README.md` becomes `.web-docs/README.md`, the plugin's front page on the
  integrations registry, while `docs/builders/index.mdx` is rendered into `docs.zip` and
  attached to each release for Packer.io. `compile-to-webdocs.sh` skips `index.mdx`, so
  that page never reaches the registry and neither page can point at the other with a
  repository-relative link. The two use different heading depths, so each page keeps its
  own headings and `@include`s only the heading-free bodies between them.

```sh
make generate      # regenerates every file marked "No" above
git diff --stat    # the result should be committed alongside the source change
```

Generated files are committed deliberately: the HashiCorp integrations portal builds
from `.web-docs/`, so it has to exist in the repository rather than being produced at
release time.

## Special thanks

Thanks @ddelnano for the community support.