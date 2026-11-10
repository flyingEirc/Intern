const dateTime = new Intl.DateTimeFormat("zh-CN", {
  dateStyle: "medium",
  timeStyle: "short",
});

/** Local rendering of a contract date-time; empty values stay empty. */
export function formatDateTime(value: string | null | undefined): string {
  if (!value) {
    return "—";
  }
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : dateTime.format(parsed);
}

const byteUnits = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

/**
 * Binary byte size. The contract carries int64 sizes as bigint after Zod
 * validation, so values past 2^53 stay exact.
 */
export function formatBytes(value: bigint | number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—";
  }
  let amount = typeof value === "bigint" ? value : BigInt(value);
  if (amount < BigInt(0)) {
    return String(value);
  }
  let unit = 0;
  const step = BigInt(1024);
  while (amount >= step && unit < byteUnits.length - 1) {
    amount /= step;
    unit += 1;
  }
  return `${amount.toString()} ${byteUnits[unit]}`;
}

/** Decimal text of an LSN; the contract uses an empty string when unset. */
export function formatLsn(value: string): string {
  return value === "" ? "—" : value;
}

const ageUnits: Array<{ limit: number; unit: string }> = [
  { limit: 60, unit: "秒" },
  { limit: 60, unit: "分钟" },
  { limit: 24, unit: "小时" },
  { limit: Number.POSITIVE_INFINITY, unit: "天" },
];

/**
 * How far `at` is from `now`, in whole units. A point newer than `now` reads
 * as just now: protection points come from completed backups, so a small clock
 * skew must not invent a negative age.
 */
export function formatAge(
  at: string,
  now: Date,
): { text: string; stale: boolean } {
  const parsed = new Date(at);
  if (Number.isNaN(parsed.getTime())) {
    return { text: at, stale: false };
  }
  let seconds = Math.floor((now.getTime() - parsed.getTime()) / 1000);
  if (seconds < 0) {
    seconds = 0;
  }
  let amount = seconds;
  let unit = "秒";
  for (const step of ageUnits) {
    if (amount < step.limit) {
      unit = step.unit;
      break;
    }
    amount = Math.floor(amount / step.limit);
    unit = step.unit;
  }
  return { text: `${amount} ${unit}前`, stale: seconds >= 24 * 60 * 60 };
}

/** One instant rendered in one IANA time zone, named so the zone is explicit. */
export function formatInTimeZone(value: string, timeZone: string): string {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return value;
  }
  const text = new Intl.DateTimeFormat("zh-CN", {
    dateStyle: "medium",
    timeStyle: "medium",
    timeZone,
  }).format(parsed);
  return `${text}（${timeZone}）`;
}

/**
 * One instant rendered in an IANA time zone and in UTC, so a scheduled fire
 * can be read in the policy's own zone without hiding the UTC the scheduler
 * stores. The two zones stay separate so a narrow column can wrap between
 * them instead of truncating one line.
 */
export function formatZonedDateTime(value: string, timeZone: string): string {
  return `${formatInTimeZone(value, timeZone)} / ${formatInTimeZone(value, "UTC")}`;
}
