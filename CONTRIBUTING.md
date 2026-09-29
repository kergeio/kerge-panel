# Contributing

Thanks for your interest in Kerge.

## Licensing

This repository is licensed under the GNU Affero General Public License v3.0 (see `LICENSE`).
Contributions are accepted under the same license.

## Developer Certificate of Origin

Every commit must carry a `Signed-off-by` line certifying the Developer
Certificate of Origin (see the `DCO` file):

```
git commit -s
```

The line must match the author of the commit:

```
Signed-off-by: Jane Doe <jane@example.com>
```

CI rejects a change when any of its commits lacks this line.

There is no CLA to sign.

## What belongs here

This repository holds the panel: the web interface, authentication,
storage, the agent endpoint, reminders and traffic accounting.

Anything that defines how the data an agent reports is to be read lives
in [kerge-protocol](https://github.com/kergeio/kerge-protocol) under the
Apache License 2.0, and must be added there rather than here. That
includes message formats and validation, interface matching rules, and
turning counters into rates and increments.

Contributions to this repository stay under the AGPL and are not moved
into differently licensed code bases.

## Before opening a pull request

```
make check
```

That runs formatting, `go vet`, the tests with the race detector, a
vulnerability scan, and the checks described below.

## House rules

- Everything in this repository is written in English: code, comments,
  commit messages, branch names and documentation.
- Commit messages follow Conventional Commits, for example
  `feat(protocol): add the host_info interval field`.
- User-facing text goes through the translation catalog in
  `web/locales/`; nothing is hard-coded.
- Run `make css` after changing style classes in the templates, and
  commit the result.
