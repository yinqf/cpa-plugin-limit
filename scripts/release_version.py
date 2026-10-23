"""Select an idempotent patch release for the checked-out commit."""

import argparse
from pathlib import Path
import re
import subprocess


VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")


def git(repo, *args):
    return subprocess.check_output(["git", "-C", str(repo), *args], text=True).strip()


def release_version(repo, explicit_tag=""):
    head = git(repo, "rev-parse", "HEAD")
    history = git(repo, "rev-list", "--first-parent", "HEAD").splitlines()
    distances = {commit: distance for distance, commit in enumerate(history)}
    tags = {}
    for tag in git(repo, "tag", "--list").splitlines():
        match = VERSION.fullmatch(tag)
        if match:
            commit = git(repo, "rev-parse", f"refs/tags/{tag}^{{commit}}")
            tags[tag] = (tuple(map(int, match.groups())), commit)

    if explicit_tag:
        if explicit_tag not in tags:
            raise ValueError("The explicit release tag must be an existing vX.Y.Z tag")
        if tags[explicit_tag][1] != head:
            raise ValueError("The explicit release tag does not point to the checked-out commit")
        return explicit_tag, False

    existing = [(version, tag) for tag, (version, commit) in tags.items() if commit == head]
    if existing:
        return max(existing)[1], False

    ancestors = [(version, tag) for tag, (version, commit) in tags.items() if commit in distances]
    if ancestors:
        (major, minor, patch), base_tag = max(ancestors)
        distance = distances[tags[base_tag][1]]
    else:
        major, minor, patch, distance = 0, 0, 0, len(history)
    # Commit distance keeps versions ordered even if queued runs start out of order.
    tag = f"v{major}.{minor}.{patch + distance}"
    if tag in tags:
        raise ValueError(f"{tag} belongs to another commit; choose an explicit version tag")
    return tag, True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    parser.add_argument("--tag", default="")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    try:
        tag, create = release_version(args.repo, args.tag)
    except (ValueError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"Release version selection failed: {error}\n")
    if args.output:
        with args.output.open("a", encoding="utf-8") as output:
            output.write(f"tag={tag}\ncreate_tag={str(create).lower()}\n")
    print(f"Release {tag} ({'new tag' if create else 'reuse existing tag'})")


if __name__ == "__main__":
    main()
