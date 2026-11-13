# Security policy

## Reporting a vulnerability

Please report security issues **privately** using GitHub's private vulnerability reporting:
open the repository's **Security** tab and choose **Report a vulnerability**. Do not open a
public issue or pull request for something exploitable.

## What is in scope

- The API (authentication, app credentials, rate limiting, submission and moderation flows,
  image upload).
- The GitHub Actions workflows, particularly the deploy workflow and anything that handles
  cloud credentials.
- Data integrity: ways to get unreviewed or tampered content published, or to corrupt or
  delete existing data.

Out of scope: volumetric denial-of-service, findings that need a compromised maintainer
account, and the third-party services we run on (report those to the provider).

## What to expect

This is a solo-maintainer background project. I will aim to acknowledge a report within a
week and to keep you updated, but I cannot promise fix timelines. Please give me a reasonable
chance to fix an issue before disclosing it publicly. Good-faith research is welcome.
