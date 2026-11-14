# Supplemental dependency licenses

Publication remains blocked by the four unresolved upstream legal texts listed below.
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
| `@epic-web/invariant@1.0.0` | [Release README](https://raw.githubusercontent.com/epicweb-dev/invariant/547be2246d3c39f8ca44dd0502b1aa70bf4378af/README.md) |
| `am-i-vibing@0.5.0` | [Release README](https://raw.githubusercontent.com/ascorbic/am-i-vibing/a858d9865e3f5a278504aad0a775a8b7a8d70bbc/packages/am-i-vibing/README.md) |
| `piccolore@0.1.3` | [Release manifest](https://raw.githubusercontent.com/delucis/piccolore/5af9fa6834292e3cb3a2bc778e4057517c1505e0/package.json) |
| `process-ancestry@0.1.0` | [Release README](https://raw.githubusercontent.com/ascorbic/process-ancestry/45004fd6155437eef40a632877a567db1b9addc5/packages/process-ancestry/README.md) |

Obtain authentic upstream legal texts or an approved dependency change before
publishing. Do not waive these missing texts because the other entries resolve.
