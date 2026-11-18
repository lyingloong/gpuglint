# gpuglint prepends a string literal containing its metrics program. Prefer the
# current interpreter, then the Python belonging to the nvitop command on PATH.
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys


def launcher_python():
    command = shutil.which("nvitop")
    if not command:
        return None
    launcher = Path(command).resolve()
    directory = launcher.parent
    python = directory / ("python.exe" if os.name == "nt" else "python")
    if python.is_file() and (directory.parent / "pyvenv.cfg").is_file():
        # pipx and uv generally link their exposed command to the environment.
        # Keep the interpreter symlink intact so Python detects its venv.
        return python
    if os.name == "posix":
        with launcher.open("rb") as stream:
            first_line = stream.readline(4096)
        if first_line.startswith(b"#!"):
            path = Path(os.fsdecode(first_line[2:].strip()))
            if path.is_absolute() and path.name.startswith("python") and path.is_file():
                return path
    return None


if importlib.util.find_spec("nvitop") is None:
    interpreter = launcher_python()
    if interpreter is not None:
        # An argument vector also handles spaces in the environment's path.
        process = subprocess.run([str(interpreter), "-"], input=metrics_source.encode("utf-8"))
        sys.exit(process.returncode)
exec(compile(metrics_source, "<gpuglint-nvitop>", "exec"))
