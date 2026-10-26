import importlib.util
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name("sanitize_test_workflows.py")
spec = importlib.util.spec_from_file_location("sanitize_test_workflows", SCRIPT)
sanitizer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sanitizer)


TEST_WORKFLOW = '''name: Test
jobs:
  test:
    steps:
      - uses: actions/setup-go@v7
        with:
          go-version: stable
      - uses: actions/cache@v4
        with:
          path: ~/.cache/go-build
      - name: Run tests
        run: make test-backend-all
  frontend:
    steps:
      - uses: actions/setup-node@v7
        with:
          node-version: '24'
          cache: pnpm
          cache-dependency-path: frontend/pnpm-lock.yaml
      - uses: actions/upload-artifact@v4
        with:
          name: junit
      - name: Test
        run: pnpm test:unit
'''

DOCKER_WORKFLOW = '''name: Docker build (shared)
jobs:
  build:
    steps:
      - name: Build image
        uses: docker/build-push-action@v7
        with:
          push: ${{ inputs.mode != 'test' }}
          cache-from: type=gha,scope=axonhub-${{ matrix.arch }}
          cache-to: type=gha,mode=${{ inputs.mode == 'test' && 'min' || 'max' }}
'''


class SanitizerTests(unittest.TestCase):
    def test_test_workflow_keeps_tests_but_disables_all_upload_caches(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".github/workflows").mkdir(parents=True)
            (root / ".github/workflows/test.yml").write_text(TEST_WORKFLOW)
            (root / ".github/workflows/docker-build.yml").write_text(DOCKER_WORKFLOW)

            sanitizer.sanitize(root)
            result = (root / ".github/workflows/test.yml").read_text()

            self.assertIn("run: make test-backend-all", result)
            self.assertIn("run: pnpm test:unit", result)
            self.assertIn("cache: false", result)
            self.assertNotIn("cache: pnpm", result)
            self.assertNotIn("actions/cache@", result)
            self.assertNotIn("actions/upload-artifact@", result)

    def test_docker_test_mode_does_not_push_or_upload_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".github/workflows").mkdir(parents=True)
            (root / ".github/workflows/test.yml").write_text(TEST_WORKFLOW)
            docker = root / ".github/workflows/docker-build.yml"
            docker.write_text(DOCKER_WORKFLOW)

            sanitizer.sanitize(root)
            first = docker.read_text()
            sanitizer.sanitize(root)
            second = docker.read_text()

            self.assertEqual(first, second)
            self.assertIn("push: ${{ inputs.mode != 'test' }}", second)
            self.assertIn("DOCKER_BUILD_RECORD_UPLOAD: ${{ inputs.mode != 'test' }}", second)
            self.assertIn("DOCKER_BUILD_SUMMARY: ${{ inputs.mode != 'test' }}", second)
            self.assertIn("cache-to: ${{ inputs.mode != 'test'", second)
            self.assertIn("cache-from: type=gha", second)


if __name__ == "__main__":
    unittest.main()
