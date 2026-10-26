#!/usr/bin/env python3
"""Disable GitHub uploads performed by upstream test workflows in the fork."""
from __future__ import annotations

import argparse
import re
from pathlib import Path


class SanitizationError(RuntimeError):
    pass


def _step_blocks(lines: list[str]):
    starts = []
    for index, line in enumerate(lines):
        match = re.match(r"^( *)-\s+(?:name|uses):", line)
        if match:
            starts.append((index, len(match.group(1))))
    for position, (start, indent) in enumerate(starts):
        end = starts[position + 1][0] if position + 1 < len(starts) else len(lines)
        yield start, end, indent


def _step_content_indent(step_indent: int) -> int:
    return step_indent + 2


def _with_child_indent(step_indent: int) -> int:
    return step_indent + 4


def _replace_step_field(lines: list[str], start: int, end: int, key: str, value: str) -> bool:
    step_indent = len(lines[start]) - len(lines[start].lstrip(" "))
    field_indent = _step_content_indent(step_indent)
    child_indent = _with_child_indent(step_indent)
    with_index = next(
        (i for i in range(start, end)
         if len(lines[i]) - len(lines[i].lstrip(" ")) == field_indent
         and re.match(r"^\s*with:\s*(?:#.*)?$", lines[i])),
        None,
    )
    if with_index is None:
        uses_index = next(
            (i for i in range(start, end) if re.match(r"^\s*(?:-\s+)?uses:", lines[i])),
            None,
        )
        if uses_index is None:
            raise SanitizationError(f"step at line {start + 1} has no uses or with block")
        lines.insert(uses_index + 1, " " * field_indent + "with:")
        # Caller re-runs the operation after a list insertion to avoid stale indices.
        raise _RetryStepScan

    pattern = re.compile(rf"^ {{{child_indent}}}{re.escape(key)}:\s*.*$")
    matching = [i for i in range(with_index + 1, end) if pattern.match(lines[i])]
    replacement = " " * child_indent + f"{key}: {value}"
    if matching:
        first = matching[0]
        changed = lines[first] != replacement
        lines[first] = replacement
        for extra in reversed(matching[1:]):
            del lines[extra]
            end -= 1
        return changed or len(matching) > 1

    # Insert the setting before the next step-level key following with:.
    insert_at = with_index + 1
    while insert_at < end:
        line = lines[insert_at]
        stripped = line.strip()
        indent = len(line) - len(line.lstrip(" "))
        if not stripped or (not stripped.startswith("#") and indent < child_indent):
            break
        insert_at += 1
    lines.insert(insert_at, replacement)
    raise _RetryStepScan


class _RetryStepScan(Exception):
    pass


def _mutate_action_steps(text: str, action_pattern: str, transform) -> tuple[str, int]:
    lines = text.splitlines()
    changed_count = 0
    while True:
        retry = False
        try:
            for start, end, _ in list(_step_blocks(lines)):
                block = "\n".join(lines[start:end])
                uses = re.search(r"(?m)^\s*(?:-\s+)?uses:\s*([^\s#]+)", block)
                if uses and re.search(action_pattern, uses.group(1), re.IGNORECASE):
                    if transform(lines, start, end):
                        changed_count += 1
            return "\n".join(lines) + "\n", changed_count
        except _RetryStepScan:
            changed_count += 1
            retry = True
        if retry:
            continue


def _disable_test_workflow_uploads(path: Path) -> bool:
    if not path.exists():
        raise SanitizationError(f"required test workflow is missing: {path}")
    original = path.read_text(encoding="utf-8")
    text = original

    # setup-go enables module/build cache upload by default; opt out explicitly.
    text, _ = _mutate_action_steps(
        text,
        r"^actions/setup-go@",
        lambda lines, start, end: _replace_step_field(lines, start, end, "cache", "false"),
    )
    # Node/Python setup actions only cache when the optional cache input is set.
    for action_pattern in (r"^actions/setup-node@", r"^actions/setup-python@"):
        text, _ = _mutate_action_steps(
            text,
            action_pattern,
            lambda lines, start, end: _remove_step_fields(
                lines, start, end, {"cache", "cache-dependency-path"}
            ),
        )

    # Cache actions save on post-job by default. Remove cache and upload-artifact
    # steps entirely from this test-only workflow while keeping actual test steps.
    lines = text.splitlines()
    removed_lines: set[int] = set()
    for start, end, _ in _step_blocks(lines):
        block = "\n".join(lines[start:end])
        uses = re.search(r"(?m)^\s*(?:-\s+)?uses:\s*([^\s#]+)", block)
        if uses and re.search(r"(?:^actions/cache(?:/[^@]+)?@|upload-artifact(?:/[^@]+)?@)", uses.group(1), re.IGNORECASE):
            removed_lines.update(range(start, end))
    text = "\n".join(line for index, line in enumerate(lines) if index not in removed_lines) + "\n"

    if not re.search(r"(?m)^\s*cache:\s*false\s*$", text):
        raise SanitizationError("test workflow has no explicit setup-go cache opt-out")
    if re.search(r"(?m)^\s*uses:\s*actions/(?:cache(?:/[^@]+)?|upload-artifact)@", text, re.IGNORECASE):
        raise SanitizationError("test workflow still contains a cache or artifact upload action")
    if re.search(r"(?m)^\s*cache:\s*(?:true|pnpm|npm|yarn|pip)\s*$", text, re.IGNORECASE):
        raise SanitizationError("test workflow still enables an optional dependency cache")

    path.write_text(text, encoding="utf-8", newline="\n")
    return text != original


def _remove_step_fields(lines: list[str], start: int, end: int, keys: set[str]) -> bool:
    step_indent = len(lines[start]) - len(lines[start].lstrip(" "))
    child_indent = _with_child_indent(step_indent)
    key_pattern = re.compile(rf"^ {{{child_indent}}}([A-Za-z0-9_-]+):")
    removed = False
    for index in range(end - 1, start, -1):
        match = key_pattern.match(lines[index])
        if match and match.group(1) in keys:
            del lines[index]
            removed = True
    return removed


def _sanitize_docker_workflow(path: Path) -> bool:
    if not path.exists():
        raise SanitizationError(f"required shared Docker workflow is missing: {path}")
    original = path.read_text(encoding="utf-8")
    lines = original.splitlines()
    blocks = list(_step_blocks(lines))
    docker_blocks = [
        (start, end)
        for start, end, _ in blocks
        if re.search(r"(?m)^\s*(?:-\s+)?uses:\s*docker/build-push-action@", "\n".join(lines[start:end]))
    ]
    if not docker_blocks:
        raise SanitizationError("shared Docker workflow has no docker/build-push-action step")

    # Insert/update env policy. Re-scan after a missing block insertion because
    # indices shift; each iteration changes at least one line or ends.
    for env_key in ("DOCKER_BUILD_RECORD_UPLOAD", "DOCKER_BUILD_SUMMARY"):
        updated, _ = _mutate_action_steps(
            "\n".join(lines) + "\n",
            r"^docker/build-push-action@",
            lambda current, start, end, key=env_key: _replace_step_env(
                current, start, end, key, "${{ inputs.mode != 'test' }}"
            ),
        )
        lines = updated.splitlines()

    # The test mode may use existing cache (cache-from) but must not export one.
    updated, _ = _mutate_action_steps(
        "\n".join(lines) + "\n",
        r"^docker/build-push-action@",
        lambda current, start, end: _replace_step_field(
            current, start, end, "cache-to",
            "${{ inputs.mode != 'test' && format('type=gha,mode=max,scope=axonhub-{0}', matrix.arch) || '' }}",
        ),
    )
    text = updated

    if not re.search(r"(?m)^\s*push:\s*\$\{\{ inputs\.mode != 'test' \}\}\s*$", text):
        raise SanitizationError("Docker test mode must keep image push disabled")
    if not re.search(r"(?m)^\s*DOCKER_BUILD_RECORD_UPLOAD:\s*\$\{\{ inputs\.mode != 'test' \}\}\s*$", text):
        raise SanitizationError("Docker test mode must disable build-record uploads")
    if not re.search(r"(?m)^\s*DOCKER_BUILD_SUMMARY:\s*\$\{\{ inputs\.mode != 'test' \}\}\s*$", text):
        raise SanitizationError("Docker test mode must disable GitHub build summaries")
    if not re.search(r"(?m)^\s*cache-to:\s*\$\{\{ inputs\.mode != 'test'", text):
        raise SanitizationError("Docker test mode must not export GitHub Actions cache")

    path.write_text(text, encoding="utf-8", newline="\n")
    return text != original


def _replace_step_env(lines: list[str], start: int, end: int, key: str, value: str) -> bool:
    step_indent = len(lines[start]) - len(lines[start].lstrip(" "))
    field_indent = _step_content_indent(step_indent)
    child_indent = _with_child_indent(step_indent)
    env_index = next(
        (i for i in range(start, end)
         if len(lines[i]) - len(lines[i].lstrip(" ")) == field_indent
         and re.match(r"^\s*env:\s*(?:#.*)?$", lines[i])),
        None,
    )
    if env_index is None:
        uses_index = next((i for i in range(start, end) if re.match(r"^\s*(?:-\s+)?uses:", lines[i])), None)
        if uses_index is None:
            raise SanitizationError("docker build step is missing uses field")
        lines[uses_index + 1:uses_index + 1] = [
            " " * field_indent + "env:",
            " " * child_indent + f"{key}: {value}",
        ]
        raise _RetryStepScan

    key_pattern = re.compile(rf"^ {{{child_indent}}}{re.escape(key)}:\s*.*$")
    matches = [i for i in range(env_index + 1, end) if key_pattern.match(lines[i])]
    replacement = " " * child_indent + f"{key}: {value}"
    if matches:
        changed = lines[matches[0]] != replacement or len(matches) > 1
        lines[matches[0]] = replacement
        for extra in reversed(matches[1:]):
            del lines[extra]
        return changed
    lines.insert(env_index + 1, replacement)
    raise _RetryStepScan


def sanitize(repo_root: Path) -> bool:
    workflows = repo_root / ".github" / "workflows"
    changed_test = _disable_test_workflow_uploads(workflows / "test.yml")
    changed_docker = _sanitize_docker_workflow(workflows / "docker-build.yml")
    return changed_test or changed_docker


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo-root", type=Path, default=Path.cwd())
    args = parser.parse_args()
    changed = sanitize(args.repo_root.resolve())
    print("Applied fork test-upload policy." if changed else "Fork test-upload policy already applied.")


if __name__ == "__main__":
    main()
