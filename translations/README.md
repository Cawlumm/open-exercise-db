# Draft machine translations

`es.machine.json` is a Spanish (`es`) draft of every published exercise's name and
instruction steps as of the seed import, written by Claude from the English text. It is
loaded as `origin='machine'` rows, which the API reports as machine-translated; a human
translation submitted through the normal flow supersedes it, and the importer never
overwrites one.

Each item carries `en_name`, the English name it was written against. The importer skips an
item whose English name or step count no longer matches the exercise's current revision, so
re-running it after the English text is edited does not attach a stale draft.

```bash
cd backend
DATABASE_URL=... go run ./cmd/import-translations -file ../translations/es.machine.json -locale es -dry-run
DATABASE_URL=... go run ./cmd/import-translations -file ../translations/es.machine.json -locale es
```

The dry run does everything but commit and prints the counts. Re-running is safe: unchanged
drafts are left alone and a changed one supersedes the old row rather than deleting it.

Style used: neutral Latin American Spanish, informal "tú" imperative, gym vocabulary as
Spanish-speaking lifters say it (Press de banca, Peso muerto, mancuerna, polea).
