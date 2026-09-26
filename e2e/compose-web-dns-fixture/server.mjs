import http from 'node:http';
import { createHash } from 'node:crypto';

const marker = process.env.MARKER || 'api';
const server = http.createServer((request, response) => {
  response.writeHead(200, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify({
    marker,
    target: request.url,
    headers: request.headers,
  }));
});

server.on('upgrade', (request, socket) => {
  const key = request.headers['sec-websocket-key'];
  if (request.url.split('?')[0] !== '/api/ws' ||
      request.headers.upgrade?.toLowerCase() !== 'websocket' ||
      request.headers['sec-websocket-version'] !== '13' ||
      typeof key !== 'string') {
    socket.end('HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n');
    return;
  }
  const guid = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';
  const accept = createHash('sha1').update(key + guid).digest('base64');
  const headers = [
    'HTTP/1.1 101 Switching Protocols',
    'Upgrade: websocket',
    'Connection: Upgrade',
    `Sec-WebSocket-Accept: ${accept}`,
    `X-Echo-Marker: ${marker}`,
    `X-Echo-Target-B64: ${Buffer.from(request.url).toString('base64')}`,
    `X-Echo-Host-B64: ${Buffer.from(request.headers.host || '').toString('base64')}`,
    `X-Echo-Origin-B64: ${Buffer.from(request.headers.origin || '').toString('base64')}`,
    `X-Echo-Xff-B64: ${Buffer.from(request.headers['x-forwarded-for'] || '').toString('base64')}`,
    `X-Echo-Real-IP-B64: ${Buffer.from(request.headers['x-real-ip'] || '').toString('base64')}`,
    '',
    '',
  ];
  socket.write(headers.join('\r\n'));
  socket.end(Buffer.from([0x88, 0x00]));
});

server.listen(8080, '0.0.0.0');
