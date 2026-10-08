"""Select a stable SemVer tag from git history; never mutate the repository."""

import re
import subprocess


STABLE = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
HEADER = re.compile(r"^[a-z]+(?:\([^\n()]+\))?(!)?: ")
FEATURE = re.compile(r"^feat(?:\([^\n()]+\))?: ")
BREAKING = re.compile(r"^BREAKING[ -]CHANGE: ", re.MULTILINE)


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def select_version():
    tags = []
    for tag in git("tag", "--list").splitlines():
        match = STABLE.fullmatch(tag)
        if match:
            tags.append((tuple(map(int, match.groups())), tag))
    if not tags:
        return {"version": "v0.1.0", "previous": "", "skip": "false"}

    (major, minor, patch), previous = max(tags)
    if git("rev-parse", previous + "^{commit}") == git("rev-parse", "HEAD"):
        # Reuse the tag after an interrupted publish, without bumping again.
        return {"version": previous, "previous": "", "skip": "false"}

    ancestor = subprocess.run(
        ["git", "merge-base", "--is-ancestor", previous, "HEAD"], check=False
    )
    if ancestor.returncode == 1:
        newer = subprocess.run(
            ["git", "merge-base", "--is-ancestor", "HEAD", previous], check=False
        )
        if newer.returncode == 0:
            # A delayed/retried run must not release older code as latest.
            return {"skip": "true"}
        if newer.returncode != 1:
            newer.check_returncode()
        raise RuntimeError("latest stable tag is outside this branch's history")
    ancestor.check_returncode()

    messages = git("log", "--format=%B%x00", previous + "..HEAD").split("\0")
    bump = 0
    for message in messages:
        message = message.strip()
        header = HEADER.match(message)
        if (header and header.group(1)) or BREAKING.search(message):
            bump = 2
        elif FEATURE.match(message):
            bump = max(bump, 1)
    if bump == 2:
        major, minor, patch = major + 1, 0, 0
    elif bump == 1:
        minor, patch = minor + 1, 0
    else:
        patch += 1
    return {
        "version": f"v{major}.{minor}.{patch}",
        "previous": previous,
        "skip": "false",
    }


if __name__ == "__main__":
    for key, value in select_version().items():
        print(f"{key}={value}")
