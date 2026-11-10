"use client";

import { CalendarClockIcon, ClockIcon } from "lucide-react";
import { useState } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldSet,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import {
  describeFull,
  describeIncremental,
  fullCron,
  incrementalCron,
  incrementalHourChoices,
  parseFullCron,
  parseIncrementalCron,
  timezoneChoices,
  weekdayLabels,
} from "@/lib/resources/schedule";

const weekdayItems = Object.fromEntries(
  weekdayLabels.map((label, value) => [String(value), label]),
);
const monthDayItems = Object.fromEntries(
  Array.from({ length: 28 }, (_, index) => [
    String(index + 1),
    `${index + 1} 日`,
  ]),
);
const intervalItems = Object.fromEntries(
  incrementalHourChoices.map((hours) => [String(hours), `每 ${hours} 小时`]),
);

function FrequencyToggle<T extends string>({
  label,
  value,
  choices,
  onChange,
}: {
  label: string;
  value: T;
  choices: ReadonlyArray<{ value: T; label: string }>;
  onChange: (next: T) => void;
}) {
  return (
    <ToggleGroup
      aria-label={label}
      variant="outline"
      value={[value]}
      onValueChange={(next) => {
        const selected = next.at(0);
        if (selected !== undefined) {
          onChange(selected as T);
        }
      }}
      className="flex flex-wrap"
    >
      {choices.map((choice) => (
        <ToggleGroupItem key={choice.value} value={choice.value}>
          {choice.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

export function ScheduleBuilder({
  full,
  incremental,
  timezone,
  errors,
  onFullChange,
  onIncrementalChange,
  onTimezoneChange,
}: {
  full: string;
  incremental: string;
  timezone: string;
  errors: { full_cron?: string; incremental_cron?: string; timezone?: string };
  onFullChange: (cron: string) => void;
  onIncrementalChange: (cron: string) => void;
  onTimezoneChange: (value: string) => void;
}) {
  const [fullSchedule, setFullSchedule] = useState(() => parseFullCron(full));
  const [incrementalSchedule, setIncrementalSchedule] = useState(() =>
    parseIncrementalCron(incremental),
  );

  function updateFull(next: typeof fullSchedule) {
    setFullSchedule(next);
    onFullChange(fullCron(next));
  }

  function updateIncremental(next: typeof incrementalSchedule) {
    setIncrementalSchedule(next);
    onIncrementalChange(incrementalCron(next));
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <CalendarClockIcon />
          保护时间安排
        </CardTitle>
        <CardDescription>安排全量与增量备份，按下方时区执行。</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-6">
        <FieldSet>
          <FieldGroup>
            <Field>
              <FieldLabel>全量保护</FieldLabel>
              <FrequencyToggle
                label="全量保护频率"
                value={fullSchedule.frequency}
                choices={[
                  { value: "daily", label: "每天" },
                  { value: "weekly", label: "每周" },
                  { value: "monthly", label: "每月" },
                  { value: "custom", label: "自定义全量" },
                ]}
                onChange={(frequency) =>
                  updateFull({ ...fullSchedule, frequency })
                }
              />
              <FieldDescription>{describeFull(fullSchedule)}</FieldDescription>
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              {fullSchedule.frequency === "weekly" ? (
                <Field>
                  <FieldLabel htmlFor="full-weekday">星期</FieldLabel>
                  <Select
                    items={weekdayItems}
                    value={fullSchedule.weekday}
                    onValueChange={(value) =>
                      updateFull({
                        ...fullSchedule,
                        weekday: String(value),
                      })
                    }
                  >
                    <SelectTrigger id="full-weekday" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectGroup>
                        {Object.entries(weekdayItems).map(([value, label]) => (
                          <SelectItem key={value} value={value}>
                            {label}
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                </Field>
              ) : null}
              {fullSchedule.frequency === "monthly" ? (
                <Field>
                  <FieldLabel htmlFor="full-month-day">每月日期</FieldLabel>
                  <Select
                    items={monthDayItems}
                    value={fullSchedule.monthDay}
                    onValueChange={(value) =>
                      updateFull({
                        ...fullSchedule,
                        monthDay: String(value),
                      })
                    }
                  >
                    <SelectTrigger id="full-month-day" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectGroup>
                        {Object.entries(monthDayItems).map(([value, label]) => (
                          <SelectItem key={value} value={value}>
                            {label}
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                  <FieldDescription>仅列出每月都存在的日期。</FieldDescription>
                </Field>
              ) : null}
              {fullSchedule.frequency !== "custom" ? (
                <Field>
                  <FieldLabel htmlFor="full-time">执行时间</FieldLabel>
                  <Input
                    id="full-time"
                    type="time"
                    value={fullSchedule.time}
                    onChange={(event) =>
                      updateFull({
                        ...fullSchedule,
                        time: event.target.value,
                      })
                    }
                  />
                </Field>
              ) : null}
            </div>
            {fullSchedule.frequency === "custom" ? (
              <Field data-invalid={errors.full_cron ? true : undefined}>
                <FieldLabel htmlFor="full-cron">全量安排</FieldLabel>
                <Input
                  id="full-cron"
                  value={fullSchedule.cron}
                  onChange={(event) =>
                    updateFull({
                      ...fullSchedule,
                      cron: event.target.value,
                    })
                  }
                  className="font-mono"
                  aria-invalid={errors.full_cron ? true : undefined}
                  autoComplete="off"
                />
                <FieldDescription>
                  {fullSchedule.legacy
                    ? "这是原策略的兼容安排，表达式完整保留；仅在需要调整时修改。"
                    : "使用标准 5 段表达式，适用于现有日历式频率之外的安排。"}
                </FieldDescription>
                <FieldError>{errors.full_cron}</FieldError>
              </Field>
            ) : null}
          </FieldGroup>
        </FieldSet>

        <FieldSet>
          <FieldGroup>
            <Field>
              <FieldLabel>增量保护</FieldLabel>
              <FrequencyToggle
                label="增量保护频率"
                value={incrementalSchedule.frequency}
                choices={[
                  { value: "hourly", label: "每小时" },
                  { value: "daily", label: "每天" },
                  { value: "custom", label: "自定义增量" },
                ]}
                onChange={(frequency) =>
                  updateIncremental({ ...incrementalSchedule, frequency })
                }
              />
              <FieldDescription>
                {describeIncremental(incrementalSchedule)}
              </FieldDescription>
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              {incrementalSchedule.frequency === "hourly" ? (
                <Field>
                  <FieldLabel htmlFor="incremental-interval">间隔</FieldLabel>
                  <Select
                    items={intervalItems}
                    value={incrementalSchedule.everyHours}
                    onValueChange={(value) =>
                      updateIncremental({
                        ...incrementalSchedule,
                        everyHours: String(value),
                      })
                    }
                  >
                    <SelectTrigger id="incremental-interval" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectGroup>
                        {incrementalHourChoices.map((hours) => (
                          <SelectItem key={hours} value={String(hours)}>
                            每 {hours} 小时
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                </Field>
              ) : null}
              {incrementalSchedule.frequency === "hourly" ? (
                <Field>
                  <FieldLabel htmlFor="incremental-minute">
                    在第几分钟
                  </FieldLabel>
                  <Select
                    items={Object.fromEntries(
                      Array.from({ length: 60 }, (_, minute) => [
                        String(minute),
                        `第 ${minute} 分钟`,
                      ]),
                    )}
                    value={String(Number(incrementalSchedule.time.slice(3)))}
                    onValueChange={(value) =>
                      updateIncremental({
                        ...incrementalSchedule,
                        time: `00:${String(value).padStart(2, "0")}`,
                      })
                    }
                  >
                    <SelectTrigger id="incremental-minute" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectGroup>
                        {Array.from({ length: 60 }, (_, minute) =>
                          String(minute),
                        ).map((minute) => (
                          <SelectItem key={minute} value={minute}>
                            第 {minute} 分钟
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                </Field>
              ) : null}
              {incrementalSchedule.frequency === "daily" ? (
                <Field>
                  <FieldLabel htmlFor="incremental-time">执行时间</FieldLabel>
                  <Input
                    id="incremental-time"
                    type="time"
                    value={incrementalSchedule.time}
                    onChange={(event) =>
                      updateIncremental({
                        ...incrementalSchedule,
                        time: event.target.value,
                      })
                    }
                  />
                </Field>
              ) : null}
            </div>
            {incrementalSchedule.frequency === "custom" ? (
              <Field data-invalid={errors.incremental_cron ? true : undefined}>
                <FieldLabel htmlFor="incremental-cron">增量安排</FieldLabel>
                <Input
                  id="incremental-cron"
                  value={incrementalSchedule.cron}
                  onChange={(event) =>
                    updateIncremental({
                      ...incrementalSchedule,
                      cron: event.target.value,
                    })
                  }
                  className="font-mono"
                  aria-invalid={errors.incremental_cron ? true : undefined}
                  autoComplete="off"
                />
                <FieldDescription>
                  {incrementalSchedule.legacy
                    ? "这是原策略的兼容安排，表达式完整保留；仅在需要调整时修改。"
                    : "使用标准 5 段表达式。"}
                </FieldDescription>
                <FieldError>{errors.incremental_cron}</FieldError>
              </Field>
            ) : null}
          </FieldGroup>
        </FieldSet>

        <Field data-invalid={errors.timezone ? true : undefined}>
          <FieldLabel htmlFor="policy-timezone">时区</FieldLabel>
          <div className="flex items-center gap-2">
            <ClockIcon data-icon="inline-start" />
            <Input
              id="policy-timezone"
              value={timezone}
              onChange={(event) => onTimezoneChange(event.target.value)}
              list="policy-timezone-choices"
              aria-invalid={errors.timezone ? true : undefined}
              autoComplete="off"
            />
          </div>
          <datalist id="policy-timezone-choices">
            {timezoneChoices.map((choice) => (
              <option key={choice} value={choice} />
            ))}
          </datalist>
          <FieldDescription>所有备份按此时区执行。</FieldDescription>
          <FieldError>{errors.timezone}</FieldError>
        </Field>
      </CardContent>
    </Card>
  );
}
