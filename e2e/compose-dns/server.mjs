import { createHash } from "node:crypto";
import http from "node:http";
import os from "node:os";

const identity = os.hostname();
const server = http.createServer((request, response) => {
  if (request.url !== "/api/health") {
    response.writeHead(404).end();
    return;
  }
  response.writeHead(200, { "Content-Type": "text/plain" }).end(identity);
});

server.on("upgrade", (request, socket) => {
  if (request.url !== "/api/ws") {
    socket.destroy();
    return;
  }
  const accept = createHash("sha1")
    .update(request.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
    .digest("base64");
  socket.write(
    "HTTP/1.1 101 Switching Protocols\r\n" +
    "Upgrade: websocket\r\n" +
    "Connection: Upgrade\r\n" +
    "Sec-WebSocket-Accept: " + accept + "\r\n" +
    "X-API-Identity: " + identity + "\r\n\r\n",
  );
  setTimeout(() => socket.end(), 1000);
});

server.listen(8080, "0.0.0.0");
