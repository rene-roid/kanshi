#!/usr/bin/env node

const { existsSync } = require('node:fs');
const { spawnSync } = require('node:child_process');
const { join } = require('node:path');

const packageRoot = join(__dirname, '..');
const composeFile = join(packageRoot, 'docker-compose.yml');
const callerDirectory = process.cwd();
const args = process.argv.slice(2);

if (args.includes('--help') || args.includes('-h')) {
  console.log(`Usage: npx @yuuki824/kanshi [update | docker-compose arguments]

Starts Kanshi with Docker Compose, using the published image
(ghcr.io/rene-roid/kanshi). A .env file in the current directory is used when
present. Common commands:
  npx @yuuki824/kanshi              start it (pulls the image the first time)
  npx @yuuki824/kanshi update       pull the newest image and restart
  npx @yuuki824/kanshi logs -f
  npx @yuuki824/kanshi down
  npx @yuuki824/kanshi up -d --build   build from source instead of pulling

Docker and the Docker Compose v2 plugin are required.`);
  process.exit(0);
}

const dockerCheck = spawnSync('docker', ['compose', 'version'], { stdio: 'ignore' });
if (dockerCheck.error || dockerCheck.status !== 0) {
  console.error('Kanshi requires Docker with the Docker Compose v2 plugin.');
  console.error('Install Docker first, then run this command again.');
  process.exit(1);
}

const composeArgs = [
  'compose',
  '--project-directory', packageRoot,
  '--project-name', 'kanshi',
  '--file', composeFile,
];

const envFile = join(callerDirectory, '.env');
if (callerDirectory !== packageRoot && existsSync(envFile)) {
  composeArgs.push('--env-file', envFile);
}

function compose(extra) {
  const result = spawnSync('docker', composeArgs.concat(extra), { stdio: 'inherit' });
  if (result.error) {
    console.error(`Could not start Docker: ${result.error.message}`);
    process.exit(1);
  }
  return result.status ?? 1;
}

if (args.length === 0) {
  process.exit(compose(['up', '--detach']));
}
if (args.length === 1 && args[0] === 'update') {
  const pulled = compose(['pull']);
  process.exit(pulled === 0 ? compose(['up', '--detach']) : pulled);
}
process.exit(compose(args));
