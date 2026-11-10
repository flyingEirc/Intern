export type FullFrequency = "daily" | "weekly" | "monthly" | "custom";
export type IncrementalFrequency = "hourly" | "daily" | "custom";

export type FullSchedule = {
  frequency: FullFrequency;
  time: string;
  weekday: string;
  monthDay: string;
  cron: string;
  legacy: boolean;
};

export type IncrementalSchedule = {
  frequency: IncrementalFrequency;
  time: string;
  everyHours: string;
  cron: string;
  legacy: boolean;
};

export const weekdayLabels = [
  "周日",
  "周一",
  "周二",
  "周三",
  "周四",
  "周五",
  "周六",
];

export const timezoneChoices = [
  "Asia/Shanghai",
  "Asia/Tokyo",
  "Asia/Singapore",
  "Europe/London",
  "Europe/Berlin",
  "UTC",
  "America/New_York",
  "America/Los_Angeles",
] as const;
export const incrementalHourChoices = [1, 2, 3, 4, 6, 8, 12] as const;

const numeric = (value: string, minimum: number, maximum: number): boolean =>
  /^\d{1,2}$/.test(value) &&
  Number(value) >= minimum &&
  Number(value) <= maximum;

const twoDigits = (value: string): string => value.padStart(2, "0");

function timeParts(value: string): [string, string] | null {
  const match = /^(\d{1,2}):(\d{1,2})$/.exec(value);
  if (
    match === null ||
    !numeric(match[1], 0, 23) ||
    !numeric(match[2], 0, 59)
  ) {
    return null;
  }
  return [String(Number(match[1])), String(Number(match[2]))];
}

/** Maps a standard cron expression onto business choices; unknown ones stay custom and untouched. */
export function parseFullCron(cron: string): FullSchedule {
  const fields = cron.trim().split(/\s+/);
  const fallback: FullSchedule = {
    frequency: "custom",
    time: "02:00",
    weekday: "1",
    monthDay: "1",
    cron: cron.trim(),
    legacy: fields.length !== 5,
  };
  if (fields.length !== 5) {
    return fallback;
  }
  const [minute, hour, day, month, weekday] = fields;
  if (!numeric(minute, 0, 59) || !numeric(hour, 0, 23)) {
    return fallback;
  }
  const time = `${twoDigits(hour)}:${twoDigits(minute)}`;
  if (day === "*" && month === "*" && weekday === "*") {
    return { ...fallback, frequency: "daily", time, legacy: false };
  }
  if (day === "*" && month === "*" && numeric(weekday, 0, 6)) {
    return {
      ...fallback,
      frequency: "weekly",
      time,
      weekday: String(Number(weekday)),
      legacy: false,
    };
  }
  if (numeric(day, 1, 28) && month === "*" && weekday === "*") {
    return {
      ...fallback,
      frequency: "monthly",
      time,
      monthDay: String(Number(day)),
      legacy: false,
    };
  }
  return fallback;
}

/** Hourly/daily are the business presets; everything else remains an editable original expression. */
export function parseIncrementalCron(cron: string): IncrementalSchedule {
  const fields = cron.trim().split(/\s+/);
  const fallback: IncrementalSchedule = {
    frequency: "custom",
    time: "00:00",
    everyHours: "1",
    cron: cron.trim(),
    legacy: fields.length !== 5,
  };
  if (fields.length !== 5) {
    return fallback;
  }
  const [minute, hour, day, month, weekday] = fields;
  if (!numeric(minute, 0, 59)) {
    return fallback;
  }
  if (day !== "*" || month !== "*" || weekday !== "*") {
    return fallback;
  }
  if (hour === "*") {
    return {
      ...fallback,
      frequency: "hourly",
      time: `00:${twoDigits(minute)}`,
      everyHours: "1",
      legacy: false,
    };
  }
  const interval = /^\*\/(\d{1,2})$/.exec(hour);
  if (
    interval !== null &&
    incrementalHourChoices.some((hours) => hours === Number(interval[1]))
  ) {
    return {
      ...fallback,
      frequency: "hourly",
      time: `00:${twoDigits(minute)}`,
      everyHours: String(Number(interval[1])),
      legacy: false,
    };
  }
  if (numeric(hour, 0, 23)) {
    return {
      ...fallback,
      frequency: "daily",
      time: `${twoDigits(hour)}:${twoDigits(minute)}`,
      legacy: false,
    };
  }
  return fallback;
}

export function fullCron(schedule: FullSchedule): string {
  const time = timeParts(schedule.time);
  if (time === null || schedule.frequency === "custom") {
    return schedule.cron.trim();
  }
  const [hour, minute] = time;
  if (schedule.frequency === "daily") {
    return `${minute} ${hour} * * *`;
  }
  if (schedule.frequency === "weekly") {
    return `${minute} ${hour} * * ${schedule.weekday}`;
  }
  return `${minute} ${hour} ${schedule.monthDay} * *`;
}

export function incrementalCron(schedule: IncrementalSchedule): string {
  const time = timeParts(schedule.time);
  if (time === null || schedule.frequency === "custom") {
    return schedule.cron.trim();
  }
  const [hour, minute] = time;
  if (schedule.frequency === "daily") {
    return `${minute} ${hour} * * *`;
  }
  const hours = Number(schedule.everyHours);
  return `${minute} ${hours === 1 ? "*" : `*/${hours}`} * * *`;
}

export function describeFull(schedule: FullSchedule): string {
  if (schedule.frequency === "daily") {
    return `每天 ${schedule.time} 执行全量保护`;
  }
  if (schedule.frequency === "weekly") {
    return `每${weekdayLabels[Number(schedule.weekday)] ?? "周指定日"} ${schedule.time} 执行全量保护`;
  }
  if (schedule.frequency === "monthly") {
    return `每月 ${schedule.monthDay} 日 ${schedule.time} 执行全量保护`;
  }
  return `按自定义时间安排执行：${schedule.cron}`;
}

export function describeIncremental(schedule: IncrementalSchedule): string {
  if (schedule.frequency === "hourly") {
    return schedule.everyHours === "1"
      ? `每小时第 ${schedule.time.slice(3)} 分执行增量保护`
      : `每 ${schedule.everyHours} 小时第 ${schedule.time.slice(3)} 分执行增量保护`;
  }
  if (schedule.frequency === "daily") {
    return `每天 ${schedule.time} 执行增量保护`;
  }
  return `按自定义时间安排执行：${schedule.cron}`;
}

export function readableSchedule(
  kind: "full" | "incremental",
  cron: string,
): string {
  return kind === "full"
    ? describeFull(parseFullCron(cron))
    : describeIncremental(parseIncrementalCron(cron));
}
