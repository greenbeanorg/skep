import type { NavItem } from "@/app/navigation";
import { PageHeader } from "@/components/page";
import { Card, CardContent } from "@/components/ui/card";

/** An honest placeholder for a section that has no backend yet. */
export function PlannedPage({ item }: { item: NavItem }) {
  const Icon = item.icon;
  return (
    <>
      <PageHeader title={item.label} />
      <Card>
        <CardContent className="flex flex-col items-center gap-3 py-14 text-center">
          <span className="grid size-12 place-items-center rounded-full bg-accent text-accent-foreground">
            <Icon className="size-6" />
          </span>
          <div className="text-base font-medium">Not built yet</div>
          <p className="max-w-md text-sm text-muted-foreground">{item.planned?.summary}</p>
          <p className="text-xs text-muted-foreground">Planned for: {item.planned?.milestone}</p>
        </CardContent>
      </Card>
    </>
  );
}
