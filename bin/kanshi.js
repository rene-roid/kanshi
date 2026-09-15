#!/usr/bin/env node

const { existsSync } = require('node:fs');
const { spawnSync } = require('node:child_process');
const { join } = require('node:path');

const packageRoot = join(__dirname, '..');
const composeFile = join(packageRoot, 'docker-compose.yml');
const callerDirectory = process.cwd();
const args = process.argv.slice(2);

if (args.includes('--help') || args.includes('-h')) {
  console.log(`Usage: npx @yuuki824/kanshi [docker-compose arguments]

Starts Kanshi with Docker Compose. A .env file in the current directory is used
when present. Common commands:
  npx @yuuki824/kanshi
  npx @yuuki824/kanshi logs -f
  npx @yuuki824/kanshi down

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

if (args.length === 0) {
  composeArgs.push('up', '--detach', '--build');
} else {
  composeArgs.push(...args);
}

const result = spawnSync('docker', composeArgs, { stdio: 'inherit' });
if (result.error) {
  console.error(`Could not start Docker: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
