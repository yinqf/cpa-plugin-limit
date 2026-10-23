"""Exercise release allocation against real temporary Git histories."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from release_version import git, release_version


class ReleaseVersionTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory(prefix="cpa-release-test-")
        self.addCleanup(self.folder.cleanup)
        self.repo = Path(self.folder.name)
        git(self.repo, "init", "-q", "-b", "main")
        git(self.repo, "config", "user.name", "Release Test")
        git(self.repo, "config", "user.email", "release-test@example.com")
        git(self.repo, "config", "commit.gpgsign", "false")
        git(self.repo, "config", "tag.gpgsign", "false")
        hooks = self.repo / "empty-hooks"
        hooks.mkdir()
        git(self.repo, "config", "core.hooksPath", str(hooks))
        self.commit("baseline")
        git(self.repo, "tag", "v0.1.3")

    def commit(self, message):
        git(self.repo, "commit", "-q", "--allow-empty", "-m", message)
        return git(self.repo, "rev-parse", "HEAD")

    def test_next_patch_and_retry_reuse(self):
        self.commit("next")
        self.assertEqual(release_version(self.repo), ("v0.1.4", True))
        self.assertEqual(release_version(self.repo), ("v0.1.4", True))
        git(self.repo, "tag", "-a", "v0.1.4", "-m", "Release v0.1.4")
        self.assertEqual(release_version(self.repo), ("v0.1.4", False))

    def test_queued_runs_can_finish_out_of_order(self):
        earlier = self.commit("earlier")
        later = self.commit("later")
        self.assertEqual(release_version(self.repo), ("v0.1.5", True))
        git(self.repo, "tag", "v0.1.5")
        git(self.repo, "checkout", "-q", earlier)
        self.assertEqual(release_version(self.repo), ("v0.1.4", True))
        git(self.repo, "tag", "v0.1.4")
        git(self.repo, "checkout", "-q", later)
        self.assertEqual(release_version(self.repo), ("v0.1.5", False))

    def test_numeric_versions_and_explicit_minor_bump(self):
        git(self.repo, "tag", "v0.1.9")
        git(self.repo, "tag", "v0.1.10")
        git(self.repo, "tag", "v9.0.0-rc.1")
        self.commit("patch")
        self.assertEqual(release_version(self.repo), ("v0.1.11", True))
        git(self.repo, "tag", "v0.2.0")
        self.commit("after minor")
        self.assertEqual(release_version(self.repo), ("v0.2.1", True))

    def test_explicit_tag_must_exist_and_match_commit(self):
        self.assertEqual(release_version(self.repo, "v0.1.3"), ("v0.1.3", False))
        self.commit("new commit")
        for tag in ("v0.1.3", "v0.1.4", "bad-tag", "v01.1.4"):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release_version(self.repo, tag)

    def test_tag_collision_never_overwrites_another_commit(self):
        git(self.repo, "checkout", "-q", "-b", "other")
        other = self.commit("other branch")
        git(self.repo, "tag", "v0.1.4")
        git(self.repo, "checkout", "-q", "main")
        self.commit("main branch")
        with self.assertRaises(ValueError):
            release_version(self.repo)
        self.assertEqual(git(self.repo, "rev-parse", "v0.1.4^{commit}"), other)

    def test_untagged_history_has_a_deterministic_start(self):
        git(self.repo, "tag", "-d", "v0.1.3")
        self.assertEqual(release_version(self.repo), ("v0.0.1", True))

    def test_cli_writes_github_outputs_without_mutating_repository(self):
        self.commit("next")
        output = self.repo / "outputs"
        script = Path(__file__).with_name("release_version.py")
        subprocess.run([sys.executable, str(script), "--repo", str(self.repo), "--output", str(output)], check=True, capture_output=True)
        self.assertEqual(output.read_text(), "tag=v0.1.4\ncreate_tag=true\n")
        self.assertEqual(git(self.repo, "tag", "--list"), "v0.1.3")


if __name__ == "__main__":
    unittest.main()
