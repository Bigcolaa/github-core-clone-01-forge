import { spawn } from 'node:child_process';
import { copyFileSync, chmodSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

// Thin launcher: the forge itself is a statically linked Go binary committed
// alongside this file. /bot is read-only during a trial, so copy to /tmp first.
const here = dirname(fileURLToPath(import.meta.url));
const binary = process.platform === 'win32' ? 'forge-windows.exe' : 'forge-linux';
const source = join(here, binary);
if (!existsSync(source)) {
  console.error(`missing ${binary}`);
  process.exit(1);
}
const destination = join(tmpdir(), `forge-${process.pid}-${binary}`);
copyFileSync(source, destination);
chmodSync(destination, 0o755);

const child = spawn(destination, [], {
  cwd: here,
  // Async preemption uses SIGURG, which is comparatively costly in the
  // grading sandbox. Our handlers are short and yield at every I/O call, so
  // cooperative preemption is enough.
  env: { ...process.env, GODEBUG: 'asyncpreemptoff=1' },
  stdio: 'inherit',
});

let forwarding = false;
const forward = (signal: NodeJS.Signals) => {
  if (forwarding) return;
  forwarding = true;
  child.kill(signal);
};
process.on('SIGINT', forward);
process.on('SIGTERM', forward);

child.on('error', (error) => {
  console.error(error);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 0);
});
