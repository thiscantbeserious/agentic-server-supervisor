# Base image for the stack-installation tests.
#
# Every one of these tests needs the same throwaway Debian host with systemd
# and curl present. Installing them per test meant ~29 identical apt-get runs
# per suite: a few seconds each on a native runner, and around 90 seconds each
# under emulation, which is what made a local run take three times longer than
# CI for reasons that had nothing to do with the code under test.
#
# Building this once per run and reusing the layer moves that cost from
# per-test to per-suite. Docker's layer cache keeps it free across runs and
# across branches for as long as this file is unchanged.
#
# Keep this minimal: it is the floor every test starts from, not a place to
# pre-install what one test happens to need. A test requiring more installs it
# itself, so what a test depends on stays visible in the test.
FROM debian:trixie-slim AS base

RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends systemd curl \
 && rm -rf /var/lib/apt/lists/*

# A second stage carrying the packages install.sh's own step1 installs.
#
# Tests that assert on step1's behaviour ("installing …" versus "already
# installed") must NOT use this — they need to observe the install happening,
# and starting from a host where it is already done would quietly rewrite what
# they measure. It exists for tests whose subject is something else entirely,
# such as the monitoring prompt, so they do not pay for a package install they
# are not testing.
FROM base AS withpackages
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends rasdaemon lm-sensors msmtp msmtp-mta \
 && rm -rf /var/lib/apt/lists/*
