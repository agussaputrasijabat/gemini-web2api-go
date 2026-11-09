// PM2 process definition for gemini-web2api.
//
// Usage:
//   pm2 start ecosystem.config.js
//   pm2 save
//
// The app resolves ./config.json relative to its working directory, so cwd
// must stay at the repo root. __dirname keeps that true no matter where pm2
// is invoked from.
const path = require('path');

const ROOT = __dirname;

module.exports = {
  apps: [
    {
      name: 'gemini-web2api',
      script: path.join(ROOT, 'gemini-web2api'),
      cwd: ROOT,
      interpreter: 'none',

      // The proxy is a long-lived server; restarts should be rare and deliberate.
      autorestart: true,
      max_restarts: 10,
      min_uptime: '20s',
      restart_delay: 3000,
      exp_backoff_restart_delay: 200,

      // Do not fork extra copies: each instance would fight for port 8081.
      instances: 1,
      exec_mode: 'fork',

      watch: false,

      // Requests can stream for minutes; SIGINT lets the server drain.
      kill_timeout: 10000,
      listen_timeout: 10000,
      shutdown_with_message: true,

      env: {
        NODE_ENV: 'production',
      },

      error_file: path.join(ROOT, 'logs', 'pm2-error.log'),
      out_file: path.join(ROOT, 'logs', 'pm2-out.log'),
      merge_logs: true,
      time: true,
    },
  ],
};
