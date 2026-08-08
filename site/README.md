# The awarer site

This directory builds the public static site from one `awa docs export` bundle
and the small presentation layer owned here. Product documentation remains
owned by the selected binary.

## Layout

```text
site/sitegen/      static-site generator, templates, styles, and local assets
site/siteserve/    loopback server for inspecting generated output
site/dist/         generated and gitignored output
```

## Build And Inspect

```bash
just site          # export docs from a dev binary and generate site/dist
just site-serve    # serve site/dist at http://127.0.0.1:8080
```

`just site` removes and rebuilds `site/dist` as one tree. The focused site tests
run with the rest of the suite — `just test`, or `go test ./site/...` directly —
so there is no separate site gate to remember.

A development build is marked as a preview. To generate from a downloaded and
verified release binary:

```bash
AWA_BIN=/path/to/verified/awa SITE_RELEASE=1 just site
```

For direct generator diagnosis:

```bash
go run ./site/sitegen \
  --docs <export-directory> \
  --output <absent-directory> \
  --base-url <base-url>
```

## Output

sitegen writes once into a directory that does not exist. An existing output is
refused, and the tool removes, replaces, and recovers nothing: a failed build may
leave a partial directory behind, and that directory carries no meaning — delete
it and run again. `just site` does exactly that for `site/dist`, which is the one
path any recipe here touches; a direct generator run may name any absent
directory instead.

The generated directory is the complete deployable artifact. Upload it without
filtering files; every file in it answers a URL.

Deployment is separate from local generation and happens automatically when a
release is published.
