const http = require('node:http');

// Only synthetic account probes reach this local fixture; never record authentication headers.
const requests = [];
http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/_requests') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(requests));
    return;
  }
  if (req.method !== 'POST' || req.url !== '/backend-api/codex/responses') {
    res.writeHead(501, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'Endpoint outside this fixture contract' }));
    return;
  }
  requests.push({
    case: req.headers['x-routing-fixture-case'],
    routing: req.headers['x-openai-account-routing-override'] || '',
  });
  // Match the Codex SSE completion shape consumed by health and intelligence probes.
  res.writeHead(200, { 'Content-Type': 'text/event-stream' });
  res.write('data: ' + JSON.stringify({ type: 'response.output_text.delta', delta: '<html>routing fixture</html>' }) + '\n\n');
  res.end('data: ' + JSON.stringify({ type: 'response.completed', response: { status: 'completed' } }) + '\n\n');
}).listen(process.env.PORT);
