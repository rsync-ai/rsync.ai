import { redirect } from "next/navigation"

// /dashboard was a second Home with its own stat cards. Home ("/") is the one
// overview now; old links and bookmarks land there.
export default function DashboardPage() {
  redirect("/")
}
