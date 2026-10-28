const http = require('http');
let calls = 0;
http.createServer((req, res) => {
  if (req.method !== 'POST' || req.url !== '/api/v1/ingest/usage') {
    res.writeHead(501, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: 'unsupported_mock_operation' })); return;
  }
  const respond = (body) => {
    calls++;
    if (calls === 1) { res.writeHead(503); res.end(JSON.stringify({ error: 'durable_ingest_unavailable' })); return; }
    if (body.schema_version !== 1 || !Array.isArray(body.events)) { res.writeHead(400); res.end(); return; }
    if (body.events.some(event => {
      const identity = event.usage && event.usage.identity;
      return identity && (!event.usage.auth_index || typeof identity.display_name !== 'string' || !identity.display_name.trim() ||
        Object.keys(identity).some(key => !['display_name', 'provider_type'].includes(key)));
    })) { res.writeHead(422); res.end(JSON.stringify({ error: 'invalid_usage_event' })); return; }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ schema_version: 1, accepted_event_ids: body.events.map(event => event.delivery_event_id) }));
  };
  if (req.body) { respond(req.body); return; }
  let raw = ''; req.on('data', chunk => { raw += chunk; });
  req.on('end', () => { try { respond(JSON.parse(raw)); } catch { res.writeHead(400); res.end(); } });
}).listen(process.env.PORT);
