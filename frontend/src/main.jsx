import React from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, RouterProvider, Outlet } from "react-router-dom";
import "./theme.css";
import Projects from "./pages/Projects.jsx";
import Dashboard from "./pages/Dashboard.jsx";
import Traces from "./pages/Traces.jsx";
import TraceDetail from "./pages/TraceDetail.jsx";
import Datasets from "./pages/Datasets.jsx";
import DatasetDetail from "./pages/DatasetDetail.jsx";
import Experiments from "./pages/Experiments.jsx";
import ExperimentDetail from "./pages/ExperimentDetail.jsx";
import Prompts from "./pages/Prompts.jsx";
import { Sidebar } from "./components/Layout.jsx";

function Layout() {
  return (
    <div className="app">
      <Sidebar />
      <main className="main">
        <Outlet />
      </main>
    </div>
  );
}

const router = createBrowserRouter([
  {
    path: "/",
    element: <Layout />,
    children: [
      { index: true, element: <Projects /> },
      { path: "projects/:pid", element: <Dashboard /> },
      { path: "projects/:pid/traces", element: <Traces /> },
      { path: "projects/:pid/traces/:tid", element: <TraceDetail /> },
      { path: "projects/:pid/datasets", element: <Datasets /> },
      { path: "projects/:pid/datasets/:dsid", element: <DatasetDetail /> },
      { path: "projects/:pid/experiments", element: <Experiments /> },
      { path: "projects/:pid/experiments/:eid", element: <ExperimentDetail /> },
      { path: "projects/:pid/prompts", element: <Prompts /> },
    ],
  },
]);

createRoot(document.getElementById("root")).render(<RouterProvider router={router} />);
