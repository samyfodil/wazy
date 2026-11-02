# wasi-testsuite runtime adapter for wazy.
#
# test-runner loads this as a module: TEST_RUNTIME_EXE names the wazy binary,
# "wazy" on PATH by default.

import os
import shlex
import subprocess
from typing import Dict, List, Optional, Tuple

# Windows paths keep their backslashes.
WAZY = shlex.split(os.getenv("TEST_RUNTIME_EXE", "wazy"), posix=(os.name != "nt"))


def get_name() -> str:
    return "wazy"


def get_version() -> str:
    result = subprocess.run(WAZY[0:1] + ["version"], encoding="UTF-8",
                            capture_output=True, check=True)
    version = result.stdout.strip()
    return "0.0.0" if version == "dev" else version


def get_wasi_versions() -> List[str]:
    return ["wasm32-wasip1"]


def get_wasi_worlds() -> List[str]:
    return ["wasi:cli/command"]


def get_timeout_seconds() -> float:
    # A test compiles its module first, which a slow runner can take seconds on.
    return 60.0


def compute_argv(test_path: str,
                 args_env_root: Tuple[List[str], Dict[str, str], Optional[str]],
                 proposals: List[str],
                 wasi_world: str,
                 wasi_version: str) -> List[str]:
    args, env, root = args_env_root
    argv = WAZY + ["run", "-hostlogging=filesystem"]
    argv += [f"-env={k}={v}" for k, v in env.items()]
    if root:
        argv += [f"-mount={root}:/"]
    return argv + [test_path] + args
