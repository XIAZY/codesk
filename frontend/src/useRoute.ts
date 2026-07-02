import { useEffect, useState } from "react";
import { type AppRoute, parseRoute, routePath } from "./routes";

export function useRoute() {
  const [route, setRoute] = useState<AppRoute>(() => parseRoute(`${window.location.pathname}${window.location.search}`));

  useEffect(() => {
    const syncRoute = () => setRoute(parseRoute(`${window.location.pathname}${window.location.search}`));
    window.addEventListener("popstate", syncRoute);
    return () => window.removeEventListener("popstate", syncRoute);
  }, []);

  return route;
}

export function navigate(route: AppRoute, options: { replace?: boolean } = {}) {
  const path = routePath(route);
  if (`${window.location.pathname}${window.location.search}` === path) {
    return;
  }
  if (options.replace) {
    window.history.replaceState(null, "", path);
  } else {
    window.history.pushState(null, "", path);
  }
  window.dispatchEvent(new PopStateEvent("popstate"));
}
