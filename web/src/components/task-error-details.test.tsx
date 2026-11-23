import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { TaskErrorDetails } from "./task-error-details";

describe("TaskErrorDetails", () => {
  it("lists the details sorted by key, arrays joined", () => {
    render(
      <TaskErrorDetails
        details={{
          reason: "volmode_none",
          command: "zfs set volmode=dev tank/a",
          clones: ["a", "b"],
          rounds: 3,
        }}
      />,
    );
    const terms = screen.getAllByRole("term").map((e) => e.textContent);
    expect(terms).toEqual(["clones", "command", "reason", "rounds"]);
    expect(screen.getByText("zfs set volmode=dev tank/a")).toBeInTheDocument();
    expect(screen.getByText("a, b")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
  });

  it("renders nothing without details", () => {
    const { container } = render(<TaskErrorDetails details={undefined} />);
    expect(container).toBeEmptyDOMElement();
    const { container: c2 } = render(<TaskErrorDetails details={{}} />);
    expect(c2).toBeEmptyDOMElement();
  });

  it("shows markup as text", () => {
    render(
      <TaskErrorDetails
        details={{ dataset: "<img src=x onerror=alert(1)>" }}
      />,
    );
    expect(
      screen.getByText("<img src=x onerror=alert(1)>"),
    ).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
  });
});
