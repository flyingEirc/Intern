"use client";

import Link from "next/link";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Field, FieldLabel } from "@/components/ui/field";
import { Select, SelectTrigger, SelectValue } from "@/components/ui/select";

/** An unavailable choice becomes a disabled select plus a real next action. */
export function EmptyChoiceField({
  id,
  label,
  message,
  placeholder,
  href,
  action,
}: {
  id: string;
  label: string;
  message: string;
  placeholder: string;
  href: string;
  action: string;
}) {
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Select items={{}} value={null} disabled>
        <SelectTrigger id={id} disabled className="w-full">
          <SelectValue placeholder={placeholder} />
        </SelectTrigger>
      </Select>
      <Empty className="border border-dashed py-3">
        <EmptyHeader>
          <EmptyTitle className="text-sm">{message}</EmptyTitle>
          <EmptyContent>
            <Button
              nativeButton={false}
              role="link"
              variant="outline"
              size="sm"
              render={<Link href={href} />}
            >
              {action}
            </Button>
          </EmptyContent>
        </EmptyHeader>
      </Empty>
    </Field>
  );
}
