import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: './frontend',
  outputDir: '/tmp/allo-card-test-results',
  fullyParallel: true,
  use: { browserName: 'chromium', headless: true },
});
