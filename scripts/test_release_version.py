"""Exercise release decisions against real git histories, including merge commits."""

import os
import pathlib
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("release_version.py").resolve()


class ReleaseVersionTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.repo = pathlib.Path(self.directory.name)
        self.env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Release test")
        self.git("config", "user.email", "release@example.invalid")
        self.commit("Initial commit")

    def git(self, *args):
        return subprocess.check_output(
            ["git", *args], cwd=self.repo, env=self.env, text=True,
            stderr=subprocess.STDOUT,
        ).strip()

    def commit(self, message):
        self.git("commit", "--allow-empty", "-qm", message)

    def version(self):
        output = subprocess.check_output(
            [sys.executable, str(SCRIPT)], cwd=self.repo, env=self.env, text=True
        )
        return dict(line.split("=", 1) for line in output.splitlines())

    def test_first_release_ignores_nonstable_tags(self):
        for tag in ("nightly", "v1.0.0-rc.1", "v01.2.3"):
            self.git("tag", tag)
        self.assertEqual(self.version(), {
            "version": "v0.1.0", "previous": "", "skip": "false",
        })

    def test_patch_is_default_for_existing_commit_style(self):
        self.git("tag", "v0.1.0")
        self.commit("Refresh README")
        self.assertEqual(self.version()["version"], "v0.1.1")
        self.assertEqual(self.version()["previous"], "v0.1.0")

    def test_minor_and_major_reset_lower_components(self):
        self.git("tag", "v1.2.9")
        self.commit("feat(remote): Pair machines")
        self.assertEqual(self.version()["version"], "v1.3.0")
        self.commit("fix(api)!: Change protocol")
        self.assertEqual(self.version()["version"], "v2.0.0")

    def test_breaking_footer_wins_over_feature(self):
        for footer in ("BREAKING CHANGE: New protocol", "BREAKING-CHANGE: New protocol"):
            with self.subTest(footer=footer):
                self.git("tag", "-f", "v0.4.5")
                self.commit("feat: Protocol\n\n" + footer)
                self.assertEqual(self.version()["version"], "v1.0.0")

    def test_numeric_tag_order_and_rerun(self):
        self.git("tag", "v1.9.0")
        self.commit("feat: More commands")
        self.git("tag", "v1.10.0")
        self.assertEqual(self.version()["version"], "v1.10.0")
        self.commit("fix: Correct command")
        self.assertEqual(self.version()["version"], "v1.10.1")

    def test_merge_looks_at_branch_commits(self):
        self.git("tag", "v1.0.0")
        self.git("checkout", "-qb", "feature")
        self.commit("feat: Queue missions")
        self.git("checkout", "-q", "main")
        self.git("merge", "--no-ff", "-m", "Merge pull request #1", "feature")
        self.assertEqual(self.version()["version"], "v1.1.0")

    def test_old_features_do_not_bump_again(self):
        self.commit("feat!: Old breaking change")
        self.git("tag", "v2.0.0")
        self.commit("docs: Explain configuration")
        self.assertEqual(self.version()["version"], "v2.0.1")

    def test_delayed_run_skips_a_newer_release(self):
        earlier = self.git("rev-parse", "HEAD")
        self.commit("fix: Released later")
        self.git("tag", "v0.1.1")
        self.git("checkout", "-q", earlier)
        self.assertEqual(self.version(), {"skip": "true"})

    def test_unrelated_release_history_is_refused(self):
        earlier = self.git("rev-parse", "HEAD")
        self.commit("One branch")
        self.git("tag", "v1.0.0")
        self.git("checkout", "-q", earlier)
        self.commit("Another branch")
        result = subprocess.run(
            [sys.executable, str(SCRIPT)], cwd=self.repo, env=self.env,
            capture_output=True, text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("outside this branch's history", result.stderr)


if __name__ == "__main__":
    unittest.main()
