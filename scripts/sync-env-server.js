const fs = require('fs');
const path = require('path');

const rootEnv = path.resolve(__dirname, '../.env');
const rootEnvExample = path.resolve(__dirname, '../.env.example');
const targetEnv = path.resolve(__dirname, '../server/.env');

if (!fs.existsSync(rootEnv)) {
  if (fs.existsSync(rootEnvExample)) {
    fs.copyFileSync(rootEnvExample, rootEnv);
    console.log('Created root .env from .env.example, please fill in the values.');
  } else {
    console.error('Root .env not found:', rootEnv);
    process.exit(1);
  }
}

const serverKeys = [
  'PORT',
  'DATABASE_URL',
  'CASDOOR_ENDPOINT',
  'CASDOOR_CLIENT_ID',
  'CASDOOR_CLIENT_SECRET',
  'CASDOOR_ORG_NAME',
  'CASDOOR_APP_NAME',
  'CASDOOR_CALLBACK_URL',
  'LLM_PROVIDER',
  'LLM_API_KEY',
  'LLM_MODEL',
  'LLM_BASE_URL',
  'LLM_MAX_TOKENS',
  'LLM_TEMPERATURE',
  'EMBEDDING_PROVIDER',
  'EMBEDDING_API_KEY',
  'EMBEDDING_MODEL',
  'EMBEDDING_BASE_URL',
  'EMBEDDING_DIMENSIONS',
  'SEARCH_DEFAULT_LIMIT',
  'SEARCH_SIMILARITY_THRESHOLD',
  'ARTIFACT_STORAGE_PATH',
];

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

const out = [];
for (const key of serverKeys) {
  if (vars[key] !== undefined) {
    out.push(`${key}=${vars[key]}`);
  }
}

fs.writeFileSync(targetEnv, out.join('\n') + '\n');
console.log('Synced root .env to server/.env');
