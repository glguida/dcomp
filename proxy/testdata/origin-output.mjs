import { outputConnections } from "../../sdk/node/src/index.mjs";

let remaining = 2;
for await (const connection of outputConnections("documents")) {
  connection.write(connection.origin + "\n");
  for await (const data of connection) connection.write(data);
  connection.end();
  if (--remaining === 0) break;
}
