import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AuthGate } from "./AuthGate";

// A stand-in for the real console: if this ever renders while unauthenticated, the fail-closed
// contract (T-010 security fix) is broken.
function ProtectedProbe() {
  return <div data-testid="protected-console">console mounted — panels + data live here</div>;
}

function mockResponse(status: number, body: unknown = null) {
  return { ok: status >= 200 && status < 300, status, json: async () => body };
}

describe("AuthGate — fail-closed rendering (T-010)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders ONLY the login screen while unauthenticated — no protected content mounts, no data fetch beyond the session check", async () => {
    fetchMock.mockResolvedValueOnce(mockResponse(401));

    const { container } = render(
      <AuthGate>
        <ProtectedProbe />
      </AuthGate>,
    );

    await waitFor(() => expect(screen.getByRole("main", { name: "Operator sign-in" })).toBeInTheDocument());

    // The protected tree never mounted — not just hidden, absent.
    expect(screen.queryByTestId("protected-console")).not.toBeInTheDocument();
    expect(container.textContent).not.toContain("console mounted");

    // Exactly one network call happened: the session check. No panel data endpoint was touched.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toContain("/v1/whoami");
  });

  it("shows a themed full-screen sign-in: wordmark, thesis line, both credential fields, submit", async () => {
    fetchMock.mockResolvedValueOnce(mockResponse(401));

    render(
      <AuthGate>
        <ProtectedProbe />
      </AuthGate>,
    );
    await waitFor(() => expect(screen.getByRole("main", { name: "Operator sign-in" })).toBeInTheDocument());

    expect(screen.getByText("Territory Grounder")).toBeInTheDocument();
    expect(
      screen.getByText("The agent is not allowed to act on a belief it has not checked."),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Operator name")).toBeInTheDocument();
    expect(screen.getByLabelText("Operator token")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeInTheDocument();
  });

  it("on a bad credential, shows an explanatory error and never mounts the console", async () => {
    fetchMock
      .mockResolvedValueOnce(mockResponse(401)) // initial session check
      .mockResolvedValueOnce(mockResponse(401)); // POST /v1/session — bad credential

    render(
      <AuthGate>
        <ProtectedProbe />
      </AuthGate>,
    );
    await waitFor(() => expect(screen.getByLabelText("Operator name")).not.toBeDisabled());

    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Operator name"), "wrong-operator");
    await user.type(screen.getByLabelText("Operator token"), "wrong-token");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "unauthenticated — check the operator name and token",
      ),
    );
    expect(screen.queryByTestId("protected-console")).not.toBeInTheDocument();
  });

  it("mounts the protected content once an existing session is verified (e.g. a page refresh)", async () => {
    fetchMock.mockResolvedValueOnce(mockResponse(200, { source: "operator:kyriakos", mutation_enabled: false }));

    render(
      <AuthGate>
        <ProtectedProbe />
      </AuthGate>,
    );

    await waitFor(() => expect(screen.getByTestId("protected-console")).toBeInTheDocument());
    expect(screen.queryByRole("main", { name: "Operator sign-in" })).not.toBeInTheDocument();
  });

  it("mounts the protected content after a successful sign-in, and not before", async () => {
    fetchMock
      .mockResolvedValueOnce(mockResponse(401)) // initial session check — no session yet
      .mockResolvedValueOnce(mockResponse(200)) // POST /v1/session — accepted
      .mockResolvedValueOnce(mockResponse(200, { source: "operator:kyriakos", mutation_enabled: false })); // post-login whoami

    render(
      <AuthGate>
        <ProtectedProbe />
      </AuthGate>,
    );
    await waitFor(() => expect(screen.getByLabelText("Operator name")).not.toBeDisabled());
    expect(screen.queryByTestId("protected-console")).not.toBeInTheDocument();

    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Operator name"), "kyriakos");
    await user.type(screen.getByLabelText("Operator token"), "s3cr3t");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    await waitFor(() => expect(screen.getByTestId("protected-console")).toBeInTheDocument());
    expect(screen.queryByRole("main", { name: "Operator sign-in" })).not.toBeInTheDocument();
  });
});
