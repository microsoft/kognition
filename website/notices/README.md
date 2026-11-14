# Supplemental dependency licenses

Publication remains blocked by five unresolved components in Linux CI.
Windows has the three JavaScript-package gaps listed below; Linux additionally
installs two libvips shared-library packages without their native legal texts.
The manifest supplies authenticated original text for 77 exact package versions
across eight files. It does not establish that the dependency tree is fully cleared.

## Provenance

- `manifest.json` maps every covered package and version to an original legal file,
  its SHA-256 digest, and an immutable upstream commit URL.
- Release tags were resolved to full commit IDs. Source package manifests and
  published package metadata identify the corresponding versions and repositories.
- Metadata-only published file listings confirmed that all 77 covered variants
  omit standalone license texts. No native binaries were downloaded.
- Coverage includes locked Linux, macOS, Windows, Android, FreeBSD, OpenHarmony,
  and WebAssembly variants in the affected native package families, plus their
  missing-text WebAssembly runtime versions.
- Shared files were byte-identical at each recorded release commit. Original
  copyright lines, whitespace, and complete legal text remain unchanged.
- `boolbase@1.0.0` uses the author's 2015 license-addition commit
  `be0bcd8a4e917a0a5895e95b523fbbed05a64871`. Its package manifest still declares
  version `1.0.0`; its `index.js` Git blob is identical to the initial published
  version's source. No later-version license was substituted.

## Unresolved publication blockers

These release sources declare an SPDX identifier without providing the complete
original legal text. The release trees, current official repository trees, and
history for the usual license-file names did not supply an authentic full text.
They are deliberately absent from `manifest.json`; an identifier or reconstructed
copyright notice cannot replace the missing text.

| Package and version | Official release source inspected |
| --- | --- |
| `am-i-vibing@0.5.0` | [Release README](https://raw.githubusercontent.com/ascorbic/am-i-vibing/a858d9865e3f5a278504aad0a775a8b7a8d70bbc/packages/am-i-vibing/README.md) |
| `piccolore@0.1.3` | [Release manifest](https://raw.githubusercontent.com/delucis/piccolore/5af9fa6834292e3cb3a2bc778e4057517c1505e0/package.json) |
| `process-ancestry@0.1.0` | [Release README](https://raw.githubusercontent.com/ascorbic/process-ancestry/45004fd6155437eef40a632877a567db1b9addc5/packages/process-ancestry/README.md) |

Linux CI also reports:

| Package and version | Declared license | Exact release source |
| --- | --- | --- |
| `@img/sharp-libvips-linux-x64@1.3.4` | LGPL-3.0-or-later | [Package manifest](https://raw.githubusercontent.com/lovell/sharp-libvips/ebb95f8add54eee8bed840e3fb587e4cbec857d7/npm/linux-x64/package.json) |
| `@img/sharp-libvips-linuxmusl-x64@1.3.4` | LGPL-3.0-or-later | [Package manifest](https://raw.githubusercontent.com/lovell/sharp-libvips/ebb95f8add54eee8bed840e3fb587e4cbec857d7/npm/linuxmusl-x64/package.json) |

The libvips packaging project's Apache license applies to its build scripts.
Its [third-party notice table](https://raw.githubusercontent.com/lovell/sharp-libvips/ebb95f8add54eee8bed840e3fb587e4cbec857d7/THIRD-PARTY-NOTICES.md)
identifies shared-library licenses, but does not contain their complete original
legal texts. Neither file alone substitutes for those native-library notices.

Obtain authentic upstream legal texts or an approved dependency change before
publishing. Do not waive these missing texts because the other entries resolve.

## Dependency remediation

The website uses `cross-env@7.0.3`, which retains the same cross-platform
environment-variable behavior without the missing-text
`@epic-web/invariant@1.0.0` dependency. The Astro and Starlight versions remain
unchanged. An older compatible framework candidate was rejected because its
dependency audit reported high and critical vulnerabilities.

Strict failures name each installed unresolved component, version, declared
license and reason. Linux CI may install different optional components than
Windows; use that exact inventory rather than assuming matching counts.
