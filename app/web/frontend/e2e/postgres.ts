import { spawnSync } from "node:child_process";
import { chmodSync, writeFileSync } from "node:fs";
import path from "node:path";

export const postgresImage =
  "postgres:17.11-trixie@sha256:ff87138949ad9a46f32ff23bd6852b52ce024a13dffe9b4087be98292cdd3b4f";

/** Each UI suite owns an isolated real PostgreSQL container and private DSN file. */
export async function startPostgres(work: string): Promise<{
  databaseUrlFile: string;
  stop: () => void;
}> {
  const name = `intern-ui-pg-${process.pid}-${Math.random().toString(36).slice(2, 10)}`;
  const password = `pg-${Math.random().toString(36).slice(2)}-${Math.random().toString(36).slice(2)}`;
  const secret = path.join(work, "postgres-password");
  writeFileSync(secret, `${password}\n`, { mode: 0o600 });
  chmodSync(secret, 0o600);
  const launched = spawnSync(
    "docker",
    [
      "run",
      "--detach",
      "--name",
      name,
      "--publish",
      "127.0.0.1::5432",
      "--mount",
      `type=bind,source=${secret},target=/run/secrets/postgres_password,readonly`,
      "--env",
      "POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password",
      "--env",
      "POSTGRES_DB=intern",
      postgresImage,
    ],
    { encoding: "utf8", timeout: 120_000 },
  );
  if (launched.status !== 0) {
    throw new Error(
      `e2e: PostgreSQL startup failed: ${launched.stderr.trim()}`,
    );
  }
  const stop = () => {
    spawnSync("docker", ["rm", "--force", name], { encoding: "utf8" });
  };
  try {
    const deadline = Date.now() + 60_000;
    let ready = false;
    while (Date.now() < deadline) {
      const status = spawnSync(
        "docker",
        [
          "exec",
          name,
          "pg_isready",
          "--host",
          "127.0.0.1",
          "--username",
          "postgres",
          "--dbname",
          "intern",
        ],
        { encoding: "utf8", timeout: 5000 },
      );
      if (status.status === 0) {
        ready = true;
        break;
      }
      await Bun.sleep(200);
    }
    if (!ready) {
      throw new Error("e2e: PostgreSQL did not become ready");
    }
    const address = spawnSync("docker", ["port", name, "5432/tcp"], {
      encoding: "utf8",
    });
    const match = /^127\.0\.0\.1:(\d+)\s*$/.exec(address.stdout);
    if (!match) {
      throw new Error("e2e: PostgreSQL loopback port is unavailable");
    }
    const databaseUrlFile = path.join(work, "database-url");
    writeFileSync(
      databaseUrlFile,
      `postgres://postgres:${encodeURIComponent(password)}@127.0.0.1:${match[1]}/intern?sslmode=disable\n`,
      { mode: 0o600 },
    );
    chmodSync(databaseUrlFile, 0o600);
    return { databaseUrlFile, stop };
  } catch (error) {
    stop();
    throw error;
  }
}
