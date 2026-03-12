const fs = require('fs');
const path = require('path');

const rootEnv = path.resolve(__dirname, '../.env');
const targetEnv = path.resolve(__dirname, '../web/web-ui/.env.local');

if (!fs.existsSync(rootEnv)) {
  console.error('Root .env not found:', rootEnv);
  process.exit(1);
}

const lines = fs.readFileSync(rootEnv, 'utf8').split('\n');
const vars = {};
for (const line of lines) {
  const trimmed = line.trim();
  if (!trimmed || trimmed.startsWith('#')) continue;
  const idx = trimmed.indexOf('=');
  if (idx === -1) continue;
  const key = trimmed.slice(0, idx).trim();
  const value = trimmed.slice(idx + 1).trim();
  vars[key] = value;
}

const mapping = {
  DATABASE_URL: 'POSTGRES_URL',
  CASDOOR_ENDPOINT: 'NEXT_PUBLIC_CASDOOR_ENDPOINT',
  CASDOOR_CLIENT_ID: 'NEXT_PUBLIC_CASDOOR_CLIENT_ID',
  CASDOOR_CLIENT_SECRET: 'CASDOOR_CLIENT_SECRET',
  CASDOOR_ORG_NAME: 'NEXT_PUBLIC_CASDOOR_ORG_NAME',
  CASDOOR_APP_NAME: 'NEXT_PUBLIC_CASDOOR_APP_NAME',
  PORT: 'PORT',
};

const lines_out = [];
for (const [src, dst] of Object.entries(mapping)) {
  if (vars[src] !== undefined) {
    lines_out.push(`${dst}=${vars[src]}`);
  }
}

const port = vars['PORT'] || '8080';
lines_out.push(`NEXT_PUBLIC_API_BASE_URL=http://localhost:${port}`);

fs.writeFileSync(targetEnv, lines_out.join('\n') + '\n');
console.log('Synced .env to web/web-ui/.env.local');
