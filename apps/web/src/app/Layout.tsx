import {
  KeyRoundIcon,
  LibraryIcon,
  LogOutIcon,
  MenuIcon,
  PlugIcon,
  ScrollTextIcon,
  ServerIcon,
  ShieldIcon,
  UsersIcon,
  type LucideIcon,
} from 'lucide-react';
import { useState } from 'react';
import { NavLink, Outlet } from 'react-router';
import { useAuth } from '@/auth/AuthProvider.tsx';
import { ErrorAlert } from '@/components/ErrorAlert.tsx';
import { cn } from '@/lib/utils.ts';
import { useSystemTheme } from '@/lib/useSystemTheme.ts';
import { Button } from '@/components/ui/button';
import { Sheet, SheetContent, SheetTitle, SheetTrigger } from '@/components/ui/sheet';
import { strings } from '@/strings.ts';

const MEMBER_LINKS: { to: string; label: string; icon: LucideIcon }[] = [
  { to: '/catalog', label: strings.nav.catalog, icon: LibraryIcon },
  { to: '/connections', label: strings.nav.connections, icon: PlugIcon },
  { to: '/tokens', label: strings.nav.tokens, icon: KeyRoundIcon },
];

const ADMIN_LINKS: { to: string; label: string; icon: LucideIcon }[] = [
  { to: '/admin/servers', label: strings.nav.servers, icon: ServerIcon },
  { to: '/admin/users', label: strings.nav.users, icon: UsersIcon },
  { to: '/admin/calls', label: strings.nav.calls, icon: ScrollTextIcon },
  { to: '/admin/audit', label: strings.nav.audit, icon: ShieldIcon },
];

function NavItem({ to, label, icon: Icon }: { to: string; label: string; icon: LucideIcon }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        cn(
          'flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-sm hover:bg-muted',
          isActive && 'bg-muted font-medium',
        )
      }
    >
      <Icon aria-hidden className="size-4" />
      {label}
    </NavLink>
  );
}

/** Navigation shared by the sidebar and the mobile sheet; admin items only for admins. */
function Sidebar() {
  const { me, logout } = useAuth();
  const [logoutError, setLogoutError] = useState<unknown>(null);
  return (
    <div className="flex h-full flex-col gap-4 p-3">
      <div className="px-2.5 pt-1 text-lg font-semibold">{strings.app.title}</div>
      <nav aria-label={strings.nav.label} className="flex flex-1 flex-col gap-1">
        {MEMBER_LINKS.map((l) => (
          <NavItem key={l.to} {...l} />
        ))}
        {me?.role === 'admin' && (
          <>
            <div className="mt-3 px-2.5 text-xs font-medium text-muted-foreground uppercase">
              {strings.nav.admin}
            </div>
            {ADMIN_LINKS.map((l) => (
              <NavItem key={l.to} {...l} />
            ))}
          </>
        )}
      </nav>
      {logoutError !== null && <ErrorAlert error={logoutError} />}
      <div className="flex flex-col gap-1 border-t pt-3">
        {me && (
          <div className="px-2.5 text-sm">
            <div className="truncate font-medium">{me.name}</div>
            <div className="text-xs text-muted-foreground">
              {me.role === 'admin' ? strings.nav.roleAdmin : strings.nav.roleMember}
            </div>
          </div>
        )}
        <Button
          variant="ghost"
          className="justify-start"
          onClick={() => {
            setLogoutError(null);
            logout().catch(setLogoutError);
          }}
        >
          <LogOutIcon aria-hidden />
          {strings.nav.logout}
        </Button>
      </div>
    </div>
  );
}

/**
 * App shell: navigation on the left, content on the right. Below 768 px (`md`) the sidebar is
 * hidden and the same navigation opens in a `Sheet` from the top bar.
 */
export function Layout() {
  const [menuOpen, setMenuOpen] = useState(false);
  useSystemTheme();
  return (
    <div className="flex min-h-svh">
      <aside className="hidden w-60 shrink-0 border-r md:block">
        <div className="sticky top-0 h-svh">
          <Sidebar />
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center gap-2 border-b px-4 md:hidden">
          <Sheet open={menuOpen} onOpenChange={setMenuOpen}>
            <SheetTrigger
              render={<Button variant="ghost" size="icon" aria-label={strings.nav.openMenu} />}
            >
              <MenuIcon aria-hidden />
            </SheetTrigger>
            <SheetContent side="left" className="w-64 gap-0 p-0">
              <SheetTitle className="sr-only">{strings.nav.label}</SheetTitle>
              <Sidebar />
            </SheetContent>
          </Sheet>
          <span className="font-semibold">{strings.app.title}</span>
        </header>
        <main className="min-w-0 flex-1 p-4 md:p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
