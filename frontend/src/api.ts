/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { token } from "./keycloak";

const BASE = import.meta.env.VITE_API_BASE ?? "/api";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** 403 is final: the user lacks the developer role, or may not see this. */
  get isForbidden(): boolean {
    return this.status === 403;
  }
}

async function get<T>(path: string): Promise<T> {
  const accessToken = await token();
  const response = await fetch(`${BASE}${path}`, {
    headers: accessToken ? { Authorization: `Bearer ${accessToken}` } : {},
  });

  if (!response.ok) {
    let message = response.statusText;
    try {
      const body = (await response.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // Response had no JSON body; the status text stands.
    }
    throw new ApiError(response.status, message);
  }
  return (await response.json()) as T;
}

export interface Session {
  user_id: string;
  username: string;
  email: string;
  roles: string[];
  is_admin: boolean;
  exposure_tier: string;
}

export interface AspectTreeNode {
  id: string;
  name: string;
  root_id: string;
  parent_id: string;
  children: AspectTreeNode[] | null;
}

export interface OntologyFunction {
  id: string;
  name: string;
  display_name: string;
  concept_id: string;
  rdf_type: string;
}

export interface Device {
  id: string;
  name: string;
  device_type_id: string;
  connection_state: string;
  shared?: boolean;
  permissions?: Record<string, boolean>;
}

export interface DeviceList {
  devices: Device[];
  total: number;
  limit: number;
  offset: number;
}

export const api = {
  session: () => get<Session>("/session"),
  aspectTree: () => get<{ tree: AspectTreeNode[] }>("/ontology/aspect-tree"),
  functions: (rdfType: "measuring" | "controlling" = "measuring") =>
    get<{ functions: OntologyFunction[]; rdf_type: string }>(
      `/ontology/functions?rdf_type=${rdfType}`,
    ),
  devices: (search: string) =>
    get<DeviceList>(`/devices?limit=100${search ? `&search=${encodeURIComponent(search)}` : ""}`),
};
