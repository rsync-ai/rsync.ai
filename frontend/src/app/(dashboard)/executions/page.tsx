import { redirect } from "next/navigation"

// The Executions list became Runs, which lists scheduled-query and workflow runs
// beside pipeline runs. A pipeline run still opens at /executions/:id.
export default function ExecutionsPage() {
  redirect("/runs?kind=pipeline")
}
