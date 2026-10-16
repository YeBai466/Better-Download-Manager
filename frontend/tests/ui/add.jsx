import React from "react";
import { createRoot } from "react-dom/client";
import AddPage from "../../src/AddPage";
createRoot(document.getElementById("root")).render(
  <React.StrictMode>
    <AddPage />
  </React.StrictMode>,
);
