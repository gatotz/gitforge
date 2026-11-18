// gitforge entrypoint: start the Go forge binary as one process.
//
// Node 22, standard library only. The forge itself is the statically
// linked Go binary ./gitforge (built from go/ with CGO_ENABLED=0); this
// file only locates it, makes sure it can run, hands it the environment,
// and waits for it to exit, forwarding its exit code.
//
// The binary is started from a copy in the OS temp dir when the committed
// file is not directly executable (a git checkout can lose the exec bit),
// so the entry works whatever mode the file was committed with.

import { spawn } from 'node:child_process';
import { copyFile, chmod, access, constants } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = dirname(fileURLToPath(import.meta.url));
const committed = join(root, 'gitforge');

async function executable(path) {
  try {
    await access(path, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

async function main() {
  let bin = committed;
  if (!(await executable(bin))) {
    // Fall back to a temp copy we can chmod; /bot is read-only in a trial.
    const copy = join(tmpdir(), `gitforge-${process.pid}`);
    await copyFile(committed, copy);
    await chmod(copy, 0o755);
    bin = copy;
  }
  const child = spawn(bin, [], { stdio: 'inherit', env: process.env });
  const code = await new Promise((resolve) => {
    child.on('error', () => resolve(1));
    child.on('exit', (c) => resolve(c));
  });
  return code ?? 1;
}

try {
  process.exitCode = await main();
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exitCode = 1;
}
