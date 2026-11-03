// Demo host tools, shared by the Electron and Wails examples. The handlers
// run wherever they are registered: the Electron main process, or the Wails
// webview.

const orders = { A7: "shipped", B2: "processing", C9: "delivered" };

export const demoTools = [
  {
    name: "order_status",
    description: "Look up the status of an order by ID (try A7, B2 or C9)",
    inputSchema: { type: "object", properties: { id: { type: "string", description: "Order ID" } }, required: ["id"] },
    readOnly: true,
    handler: ({ id }) => ({ id, status: orders[id] ?? "unknown" }),
  },
  {
    name: "refund",
    description: "Refund an order",
    inputSchema: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
    approval: "ask", // Harness Kit asks the user before every call
    handler: ({ id }) => {
      if (!orders[id]) throw new Error(`no order ${id}`);
      orders[id] = "refunded";
      return `Order ${id} refunded`;
    },
  },
];
