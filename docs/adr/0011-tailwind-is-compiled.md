# 0011 — Tailwind is compiled once and the stylesheet is committed

## Status

Accepted. It closes the decision [0001](0001-package-layout-and-single-static-binary.md)
deferred to the UX phase.

## Context

The board was styled by Tailwind's Play CDN build, vendored as
`assets/vendor/tailwindcss/tailwind.js`: 407 kB of JavaScript that reads the
markup after it arrives, works out which utilities the page uses, and writes a
`<style>` element from script. Tailwind documents it as a development tool and
says not to use it in production.

Three things followed from that, in order of how much they cost:

- Every page load shipped 407 kB (123 kB gzipped) of JavaScript to do work that
  belongs to a build. That is more than htmx, SortableJS and our own script
  together, on a board whose selling point is that it is not a JavaScript
  application.
- The styles arrived after the first paint. `app.css` still carries the rule
  that was written to hide the white flash this caused.
- The CSP had to allow inline style because Tailwind wrote one.

The alternatives were the ones 0001 listed. Keep the Play build, which is the
cost above. Move to Bootstrap, which is a rewrite of every template for a
smaller CSS file and a jQuery-free but still scripted component set. Or compile,
which is what Tailwind is for.

## Decision

Compile once with the standalone CLI, commit the result, and serve it as an
ordinary stylesheet.

- `assets/tailwind.sh` downloads the pinned CLI (3.4.17), checks its sha256
  against a per-platform digest in the script, keeps it in `.tools/` and runs
  it. The CLI is a single binary, so the "no code generator that needs Node"
  rule in `docs/architecture.md` holds: nothing is installed, and nothing about
  it is required to build or run the app.
- `assets/tailwind.config.js` names the two places a class can come from, the
  templates and `app.js`, and sets `darkMode: 'class'` because the theme is a
  class on `<html>` that a button toggles.
- `assets/tailwind.css` is the output, 38 kB and 7 kB gzipped, embedded and
  served with the digest-stamped URL every other asset gets. `go build` needs
  nothing but Go, and the Docker build does not see the compiler or its inputs.
- `go generate ./assets/` runs the compile. CI runs the same script and fails on
  a diff, so a class that reaches a page without reaching the stylesheet is a red
  build rather than an element that quietly renders unstyled.
- Two tests in `assets/` cover what a diff cannot: one walks the repository for
  files that write class names and fails if a content glob does not cover them,
  and fails as well when a glob matches nothing at all, which is what a renamed
  directory leaves behind. The other asserts the dark variants compile to
  `.dark` selectors rather than a `prefers-color-scheme` query, because that
  swap is silent and leaves the toggle half working.
- The stylesheet is not minified. The diff of that file is how a reviewer sees
  what a new class actually added, and gzip takes most of what minifying would.

Version 3.4.17 rather than 4.x: v4 replaces the JavaScript config with CSS
directives and changes what several utilities mean. That is a second change with
its own visual review, and doing both at once would leave no way to tell which
one moved a margin. This ADR is the place to record it when it happens.

## Consequences

- The page loads 112 kB of JavaScript instead of 519 kB, and 37 kB instead of
  160 kB gzipped. Styles are there at first paint.
- Adding a class to a template now has a build step. Forgetting it is caught by
  CI rather than by looking at the board, and the fix is one command.
- The CSP still allows inline script and style, for the pre-paint theme script,
  ten inline handlers, and the `style` attribute that carries a label's colour.
  Compiling removed one of the three reasons, not the need.
- A class assembled at runtime from pieces (`'bg-' + colour`) would compile to
  nothing. Nothing does this today; the class-writing test is what keeps the
  content globs honest, not that habit.
- The custom rules in `app.css` stay where they are. It loads after Tailwind
  now, so a rule of ours wins a tie against the reset instead of relying on
  specificity.
- Upgrading Tailwind means editing one version and one digest per platform in
  `tailwind.sh`, running the compile and reading the diff of `tailwind.css`.
  Dependabot cannot do it, which is the price of not having a package manager
  here.
