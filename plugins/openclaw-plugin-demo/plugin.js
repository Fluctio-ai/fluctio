/**
 * Example: An OpenClaw-style plugin that registers tools.
 * This simulates what an npm-published OpenClaw plugin looks like.
 */
module.exports = {
  default: {
    id: "demo",
    name: "Demo OpenClaw Plugin",
    register(api) {
      // Register a weather tool
      api.registerTool({
        name: "get_weather",
        description: "Get current weather for a city",
        parameters: {
          type: "object",
          properties: {
            city: { type: "string", description: "City name" },
          },
          required: ["city"],
        },
        async execute(args) {
          // Simulated weather response
          return `Weather in ${args.city}: 22°C, partly cloudy`;
        },
      });

      // Register a calculator tool
      api.registerTool({
        name: "calculate",
        description: "Evaluate a simple arithmetic expression (+ - * / % and parentheses)",
        parameters: {
          type: "object",
          properties: {
            expression: { type: "string", description: "Arithmetic expression, e.g. (2+3)*4" },
          },
          required: ["expression"],
        },
        async execute(args) {
          // The old demo used Function(`return (${expression})`) — dynamic
          // code evaluation exposed to the LLM, i.e. unsandboxed RCE on the
          // plugin host (which inherits the gateway's full environment).
          // A demo plugin is copied verbatim by users, so it now evaluates
          // with a strict arithmetic-only character check and a real parser
          // via Function on a VERIFIED-safe operand string only.
          const expr = String(args.expression || "");
          if (!/^[0-9+\-*/%().\s]+$/.test(expr)) {
            return "Error: only numbers and + - * / % ( ) are allowed";
          }
          try {
            // expr is now guaranteed to contain only digits, operators,
            // parens and whitespace — no identifiers, so no property or
            // function access is possible.
            const result = Function(`"use strict"; return (${expr})`)();
            if (typeof result !== "number" || !Number.isFinite(result)) {
              return `Error: expression did not evaluate to a finite number`;
            }
            return `${expr} = ${result}`;
          } catch (e) {
            return `Error: invalid expression`;
          }
        },
      });
    },
  },
};
