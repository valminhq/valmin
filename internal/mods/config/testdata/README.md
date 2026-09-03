# `.cfg` corpus

The fixtures the round-trip harness runs against: parse → serialise → byte-compare
(`03 §9` rule 6). Every file here is **synthetic**. No third-party config is committed.

## Provenance

The grammar is measured; the content is invented.

| Source | What it supplied |
|---|---|
| `BepInEx/config/BepInEx.cfg`, as shipped in `denikson-BepInExPack_Valheim-5.4.2333` | Every structural shape in `plugin/BepInEx.cfg` — line endings, comment forms, metadata keys, section and key layout |
| `03 §9` | The per-plugin file header, the metadata key set, and the type list |

Section names, key names and metadata keys are reproduced verbatim where code must match
them (`[Logging.Console] Enabled` is what `EnsureConsoleLogging` looks for, so a fixture
that renamed it would assert nothing). Descriptions, values and plugin identities are
written for this corpus.

`.gitattributes` sets `* -text` so git never converts a line ending. Byte-exactness is the
entire point of these files; a checkout that normalised them would turn the harness green
and useless.

## Measured against the shipped file

Facts taken from the real file that a fixture set would not otherwise contain:

- **Line endings are mixed within one file.** 145 of its 153 lines end CRLF; 8 end LF.
  The LF lines sit inside multi-line `##` descriptions — the framework terminates its own
  lines CRLF and writes a newline embedded in a description string through as-is. A
  serialiser that detects one dominant ending per file and re-emits it corrupts the file
  that ships with every install. `plugin/BepInEx.cfg` reproduces this.
- **A key may carry no metadata at all.** `ForceBepInExTTYDriver` has no `##` description,
  no `# Setting type:` and no `# Default value:`.
- **A description line may be `## ` with a trailing space**, and trailing whitespace is
  load-bearing for a byte compare.
- **The same key name appears in several sections** (`Enabled`, `LogLevels`), so an index
  keyed on the key alone collides.
- **`# Multiple values can be set ...` appears as a free-text metadata line**, not a
  structured key.

## `03 §9` type coverage

`03 §9` lists the types "seen in the wild". Only three of them were observed in the one
real file available locally; the rest are the doc's claim, unverified here.

| Type | In the corpus | Observed in a real file |
|---|---|---|
| `Boolean` | yes | yes |
| `String` | yes | yes |
| enum (`Acceptable values`) | yes | yes — `LogChannel`, `ConsoleOutRedirectType`, `MonoModBackend` |
| flags enum (`Multiple values`) | yes | yes — `LogLevel` |
| `Int32` | yes | **no** |
| `Single` | yes | **no** |
| `Double` | yes | **no** |
| `KeyboardShortcut` | yes | **no** |
| `Color` | yes | **no** |

`⚠ verify` The five unobserved rows are modelled on `03 §9`'s examples. A plugin-generated
config would confirm the exact spelling of `# Acceptable value range: From X to Y` and of a
`Color` value. Tracked as Q45; the fifteen-mod boot in `docs/M3-VERIFICATION.md` is what
settles it.

## Files

### `plugin/` — whole files in the shape the framework writes

| File | Carries |
|---|---|
| `BepInEx.cfg` | The framework's own config: mixed line endings, a key with no metadata, `Enabled` and `LogLevels` in two sections each, `## ` with a trailing space, a free-text metadata line, a trailing blank line |
| `com.example.everysetting.cfg` | One setting per row of `03 §9`'s widget table, plus a type the parser cannot know |
| `com.example.minimal.cfg` | The smallest well-formed plugin config: header, one section, one setting |

### `edge/` — one hostile shape per file

| File | Carries |
|---|---|
| `bom.cfg` | A UTF-8 BOM ahead of the first byte |
| `crlf.cfg` | CRLF on every line |
| `duplicate-key.cfg` | A key assigned twice in one section, and the same name again in another |
| `empty-section.cfg` | Section headers with no settings under them |
| `key-before-section.cfg` | An assignment preceding any section header |
| `long-description.cfg` | A four-line `##` description ending in a blank `##` |
| `no-trailing-newline.cfg` | A final line with no terminator |
| `only-comments.cfg` | A header and no settings |
| `trailing-whitespace.cfg` | Trailing spaces and tabs on a section header, a description, a value, and padding around `=` |
| `unknown-metadata.cfg` | Metadata keys the parser does not recognise, including a setting whose only metadata is unrecognised (`03 §9` rule 4) |
| `value-with-equals.cfg` | A value containing `=`, a value that is `=`, and an empty value |
