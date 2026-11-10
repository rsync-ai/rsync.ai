"use client"

import { useEffect, useRef, useState, useCallback } from "react"
import { PageHeader } from "@/components/layout/PageHeader"
import { AdminNav } from "@/components/admin/AdminNav"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { AdminReadFailureState, LoadingState } from "@/components/admin/AdminStates"
import { toast } from "sonner"
import { MoreHorizontal, Search } from "lucide-react"
import type { AdminReadFailure, AdminUser } from "@/lib/api/admin"
import {
  adminListUsers,
  adminUpdateUserRole,
  adminUpdateUserStatus,
  adminDeleteUser,
} from "@/lib/api/admin"
import { formatDate } from "@/lib/utils"

const roleBadgeVariant: Record<string, "default" | "secondary" | "outline" | "destructive"> = {
  admin: "default",
  power_user: "secondary",
  user: "outline",
  viewer: "outline",
}

const statusBadgeVariant: Record<string, "default" | "secondary" | "outline" | "destructive"> = {
  active: "default",
  deactivated: "destructive",
}

export default function AdminUsersPage() {
  const [users, setUsers] = useState<AdminUser[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(true)
  const [failure, setFailure] = useState<AdminReadFailure | null>(null)
  // Set by the first good read. From then on the filters stay mounted
  // through every reload: swapping them for a loading card on each
  // keystroke unmounted the search box and dropped its focus.
  const [loaded, setLoaded] = useState(false)
  const latest = useRef(0)
  const [search, setSearch] = useState("")
  const [roleFilter, setRoleFilter] = useState("")
  const [statusFilter, setStatusFilter] = useState("")
  const [offset, setOffset] = useState(0)

  // Dialog state
  const [roleDialogUser, setRoleDialogUser] = useState<AdminUser | null>(null)
  const [newRole, setNewRole] = useState("")
  const [statusDialogUser, setStatusDialogUser] = useState<AdminUser | null>(null)
  const [deleteDialogUser, setDeleteDialogUser] = useState<AdminUser | null>(null)

  const load = useCallback(async () => {
    const request = ++latest.current
    setLoading(true)
    const r = await adminListUsers({ limit: 50, offset, q: search, role: roleFilter, status: statusFilter })
    // Typing sends a read per keystroke; an older one answering late must not
    // replace the newer one's rows.
    if (request !== latest.current) return
    if (r.ok) {
      setUsers(r.data.data || [])
      setTotal(r.data.total || 0)
      setFailure(null)
      setLoaded(true)
    } else {
      setFailure(r)
    }
    setLoading(false)
  }, [offset, search, roleFilter, statusFilter])

  useEffect(() => { load() }, [load])

  const handleRoleChange = async () => {
    if (!roleDialogUser || !newRole) return
    try {
      await adminUpdateUserRole(roleDialogUser.id, newRole)
      toast.success(`Role updated to ${newRole}`)
      setRoleDialogUser(null)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to update role")
    }
  }

  const handleStatusToggle = async () => {
    if (!statusDialogUser) return
    const newStatus = statusDialogUser.status === "active" ? "deactivated" : "active"
    try {
      await adminUpdateUserStatus(statusDialogUser.id, newStatus)
      toast.success(`User ${newStatus === "active" ? "activated" : "deactivated"}`)
      setStatusDialogUser(null)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to update status")
    }
  }

  const handleDelete = async () => {
    if (!deleteDialogUser) return
    try {
      await adminDeleteUser(deleteDialogUser.id)
      toast.success("User deleted")
      setDeleteDialogUser(null)
      load()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to delete user")
    }
  }

  return (
    <div className="space-y-5">
      <PageHeader heading="Admin" description="User management" />
      <AdminNav />

      {!loaded ? (
        failure && !loading ? <AdminReadFailureState failure={failure} onRetry={load} /> : <LoadingState />
      ) : (
        <Card className="p-4" aria-busy={loading}>
          <div className="flex flex-wrap items-center gap-3 mb-4">
            <div className="relative flex-1 min-w-[200px]">
              <Search className="absolute left-2.5 top-2.5 h-4 w-4 text-zinc-400" />
              <Input
                placeholder="Search by email or name..."
                value={search}
                onChange={(e) => { setSearch(e.target.value); setOffset(0) }}
                className="pl-8"
              />
            </div>
            <Select value={roleFilter} onValueChange={(v) => { setRoleFilter(v === "all" ? "" : v); setOffset(0) }}>
              <SelectTrigger className="w-[140px]">
                <SelectValue placeholder="All roles" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All roles</SelectItem>
                <SelectItem value="admin">Admin</SelectItem>
                <SelectItem value="power_user">Power User</SelectItem>
                <SelectItem value="user">User</SelectItem>
                <SelectItem value="viewer">Viewer</SelectItem>
              </SelectContent>
            </Select>
            <Select value={statusFilter} onValueChange={(v) => { setStatusFilter(v === "all" ? "" : v); setOffset(0) }}>
              <SelectTrigger className="w-[140px]">
                <SelectValue placeholder="All statuses" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All statuses</SelectItem>
                <SelectItem value="active">Active</SelectItem>
                <SelectItem value="deactivated">Deactivated</SelectItem>
              </SelectContent>
            </Select>
            <Button variant="outline" onClick={load}>Refresh</Button>
          </div>

          {failure ? (
            <AdminReadFailureState failure={failure} onRetry={load} />
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead>Email</TableHead>
                    <TableHead>Role</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead className="hidden @xl:table-cell">Created</TableHead>
                    <TableHead className="hidden @xl:table-cell">Last Login</TableHead>
                    <TableHead className="text-right">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {users.length === 0 ? (
                    <TableRow>
                      <TableCell colSpan={7} className="text-center text-sm text-zinc-500 dark:text-zinc-400 py-8">
                        No users found
                      </TableCell>
                    </TableRow>
                  ) : (
                    users.map((user) => (
                      <TableRow key={user.id}>
                        <TableCell className="font-medium">{user.name || "-"}</TableCell>
                        <TableCell className="text-sm text-zinc-600 dark:text-zinc-400">{user.email}</TableCell>
                        <TableCell>
                          <Badge variant={roleBadgeVariant[user.role] || "outline"}>{user.role}</Badge>
                        </TableCell>
                        <TableCell>
                          <Badge variant={statusBadgeVariant[user.status] || "outline"}>{user.status}</Badge>
                        </TableCell>
                        <TableCell className="hidden @xl:table-cell text-sm text-zinc-500 dark:text-zinc-400">
                          {formatDate(user.created_at)}
                        </TableCell>
                        <TableCell className="hidden @xl:table-cell text-sm text-zinc-500 dark:text-zinc-400">
                          {user.last_login ? formatDate(user.last_login) : "-"}
                        </TableCell>
                        <TableCell className="text-right">
                          <DropdownMenu>
                            <DropdownMenuTrigger asChild>
                              <Button variant="ghost" size="sm" className="h-8 w-8 p-0">
                                <MoreHorizontal className="h-4 w-4" />
                              </Button>
                            </DropdownMenuTrigger>
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem onClick={() => { setRoleDialogUser(user); setNewRole(user.role) }}>
                                Change Role
                              </DropdownMenuItem>
                              <DropdownMenuItem onClick={() => setStatusDialogUser(user)}>
                                {user.status === "active" ? "Deactivate" : "Activate"}
                              </DropdownMenuItem>
                              <DropdownMenuSeparator />
                              <DropdownMenuItem className="text-red-600" onClick={() => setDeleteDialogUser(user)}>
                                Delete
                              </DropdownMenuItem>
                            </DropdownMenuContent>
                          </DropdownMenu>
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>

              {total > 50 && (
                <div className="flex items-center justify-between mt-4 text-sm text-zinc-500 dark:text-zinc-400">
                  <span>Showing {offset + 1}-{Math.min(offset + 50, total)} of {total}</span>
                  <div className="flex gap-2">
                    <Button variant="outline" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - 50))}>
                      Previous
                    </Button>
                    <Button variant="outline" size="sm" disabled={offset + 50 >= total} onClick={() => setOffset(offset + 50)}>
                      Next
                    </Button>
                  </div>
                </div>
              )}
            </>
          )}
        </Card>
      )}

      {/* Role Change Dialog */}
      <Dialog open={!!roleDialogUser} onOpenChange={() => setRoleDialogUser(null)} closeOnOutsideClick={false}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Change Role</DialogTitle>
            <DialogDescription>
              Change role for {roleDialogUser?.email}
            </DialogDescription>
          </DialogHeader>
          <Select value={newRole} onValueChange={setNewRole}>
            <SelectTrigger aria-label="Role">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="viewer">Viewer</SelectItem>
              <SelectItem value="user">User</SelectItem>
              <SelectItem value="power_user">Power User</SelectItem>
              <SelectItem value="admin">Admin</SelectItem>
            </SelectContent>
          </Select>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRoleDialogUser(null)}>Cancel</Button>
            <Button onClick={handleRoleChange}>Update Role</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Status Toggle Dialog */}
      <AlertDialog open={!!statusDialogUser} onOpenChange={() => setStatusDialogUser(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {statusDialogUser?.status === "active" ? "Deactivate" : "Activate"} User
            </AlertDialogTitle>
            <AlertDialogDescription>
              {statusDialogUser?.status === "active"
                ? `This will deactivate ${statusDialogUser?.email} and purge all their active sessions.`
                : `This will reactivate ${statusDialogUser?.email}.`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={handleStatusToggle}>
              {statusDialogUser?.status === "active" ? "Deactivate" : "Activate"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* Delete User Dialog */}
      <AlertDialog open={!!deleteDialogUser} onOpenChange={() => setDeleteDialogUser(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete User</AlertDialogTitle>
            <AlertDialogDescription>
              This will permanently delete {deleteDialogUser?.email} and all their sessions. This action cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction className="bg-red-600 hover:bg-red-700" onClick={handleDelete}>
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
