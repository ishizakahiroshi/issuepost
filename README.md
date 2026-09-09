# issuepost

One place for the issues your apps hear about.

Several apps, one ledger. Each app posts what it hears — a crash it detected itself, or a
bug, request, question or note a person typed — to a single endpoint. The ledger keeps them
all in one table, tells you who is waiting on whom, and shows you where the trouble is
concentrated.

> **Status: early design.** Nothing is implemented yet. The name is reserved and the shape
> is being written down first.

## The problem

You run more than one internal application. Each one grows its own way of hearing about
problems: a chat thread here, a spreadsheet there, an error log nobody reads. The same
report gets a different number in each system, a different set of statuses, and a different
notion of "done". Nobody can answer "how many things are stuck, and on whom?" without
asking three people.

Splitting it further does not help. **One case is one thing, so it should live in one place.**

## The shape

- **Each app posts its own reports.** No app collects for another. The ledger stores what it
  receives without reshaping it.
- **One table for four kinds.** A crash the system detected, a bug a person reported, a
  feature request, a question — these are attributes of one record, not four systems.
  Keeping them together is what makes the numbers readable: *a screen with many questions
  is not broken, it is unclear.*
- **Context comes from the app, not the reporter.** Screen, environment, version and URL are
  attached automatically. The person writes what happened, nothing else.
- **Duplicates add a person, not a number.** The second report of the same thing joins the
  first. How many people hit it becomes the priority signal.
- **Links, not copies.** A record can point at the commit that fixed it and the document that
  explains it.
- **Failing to send is never silent.** If a report cannot reach the ledger, the app says so
  and retries later. It never tells the user "received" when it was dropped.

## Ingest

Apps authenticate with a bearer token, optionally restricted by source address. One endpoint,
same payload shape for every app and every kind of report.

## Status and roadmap

Design first, then a reference implementation and a dashboard. Follow the repository for
progress. Issues and discussion are welcome once the design document lands.

## License

Apache License 2.0. See [LICENSE](LICENSE).
