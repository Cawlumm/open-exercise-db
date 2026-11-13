<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo-light.svg" alt="open-exercise-db" height="56">
  </picture>
</p>

<p align="center">
  A community-maintained, open exercise database — <em>OpenFoodFacts, but for fitness.</em>
</p>

<p align="center">
  <img alt="code license" src="https://img.shields.io/badge/code%20license-MIT-blue">
  <img alt="data license" src="https://img.shields.io/badge/data%20license-CC0%201.0-lightgrey">
  <img alt="status" src="https://img.shields.io/badge/status-early%20but%20live-yellow">
</p>

---

## What is open-exercise-db?

### An exercise database

Exercises with their muscles, equipment, step-by-step instructions, images and translations.
About 870 to start with, in English and Spanish.

### Made by everyone

Anyone can add an exercise, fix one or translate it from inside an app that uses the
database. Every change is reviewed before it is published, and every published change can
be reverted.

### For everyone

The data is public domain ([CC0](DATA_LICENSE)), so any app can use it for anything, and the
code is [MIT](LICENSE).

## Use the data

**The whole catalog**, for apps that keep a local copy: a static snapshot, rebuilt every 6
hours and free to fetch.

```
https://cawlumm.github.io/open-exercise-db/v1/manifest.json
```

**Live queries**, for search and single lookups: the API.

```bash
curl -A "MyApp/1.0 (you@example.com)" \
  "https://oedb-api-o4dqddih5q-uk.a.run.app/api/v1/exercises?q=squat"
```

Every request needs a `User-Agent` naming your app and a contact. Interactive docs are at
[`/docs`](https://oedb-api-o4dqddih5q-uk.a.run.app/docs). Rate limits, the snapshot format and
how to ask for more are in [`docs/API_POLICY.md`](docs/API_POLICY.md).

## Contribute

- **Exercises and translations**: apps submit them on their users' behalf with an app
  credential. Credentials are issued by hand for now:
  [ask for one](https://github.com/Cawlumm/open-exercise-db/issues/new?template=rate-limit-request.yml).
- **Code**: fork, branch and open a pull request. [`CONTRIBUTING.md`](CONTRIBUTING.md) covers
  setup and tests.
- **Something wrong in the data?** [Open an issue](https://github.com/Cawlumm/open-exercise-db/issues/new/choose).

This is a one-person side project, so reviews are best-effort.

## Run it yourself

```bash
git clone https://github.com/Cawlumm/open-exercise-db.git
cd open-exercise-db && cp .env.example .env
docker compose up -d --wait
```

Then load the starter catalog and see [`docs/SELF_HOSTING.md`](docs/SELF_HOSTING.md) for the
details. How it is built and why: [`docs/DESIGN.md`](docs/DESIGN.md).

## Sources and related projects

open-exercise-db stands on other people's open work. Go check these out:

- **[yuhonas/free-exercise-db](https://github.com/yuhonas/free-exercise-db)** (Unlicense):
  the day-one seed. Its ~870 exercises and their images were imported as-is, and every
  imported revision keeps its `license` and `source_url`.
- **[wrkout/exercises.json](https://github.com/wrkout/exercises.json)** by Ollie Jennings:
  the original dataset free-exercise-db restructured.
- **[wger](https://github.com/wger-project/wger)**: a full self-hosted fitness tracker with a
  community exercise wiki. Its data is CC-BY-SA, which is not compatible with CC0, so nothing
  is imported from it; it is only a reference for spotting gaps.
- **[exercemus/exercises](https://github.com/exercemus/exercises)**: a static dataset that
  aggregates wger and free-exercise-db. Reference only, for the same reason.
- **[Open Food Facts](https://world.openfoodfacts.org)**: the model this project follows for
  community submissions, moderation and bulk data dumps.
- **Spanish (`es`) translations** are machine-drafted (by an LLM) and marked as such in the API
  (`*_machine_origin`) until a person reviews them.
- **Logo** wordmark is set in [Inter](https://rsms.me/inter/) (SIL Open Font License),
  outlined to paths.

Used by [Lyftr](https://github.com/Cawlumm/lyftr), a self-hosted workout tracker.

If you hold rights to something in the catalog and want it credited differently or removed,
open an issue.

## License

Code: [MIT](LICENSE). Data: [CC0 1.0](DATA_LICENSE), public domain, no attribution required.
