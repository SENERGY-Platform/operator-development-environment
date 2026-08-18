/*
 * Contract check, not shipped. The JSON beside this file was produced by a
 * running backend, so assigning it to the declared types fails the build on any
 * field the frontend expects and the backend does not send, or vice versa.
 *
 * `satisfies` rather than a plain annotation, because it also rejects an *extra*
 * field: a renamed field would otherwise pass as one absent optional plus one
 * unnoticed extra.
 *
 * Loose<T> widens string-literal unions to string. A JSON import types every
 * string as `string`, so without it the check fails on `confidence: "uncertain"`
 * not being the union member it plainly is — noise that would hide the mismatches
 * actually being looked for. Structure and field sets are still checked exactly.
 */
import type {
  AvailabilityList,
  LLMProfileView,
  ProfileOverrideRecord,
  ProfileResult,
  QuickProfileList,
  SelectionResult,
  SeriesProfile,
  SessionPage,
} from "../api";

import availability from "./availability.json";
import override from "./override.json";
import profile from "./profile.json";
import profiles from "./profiles.json";
import projection from "./projection.json";
import quick from "./quick.json";
import selection from "./selection.json";
import sessions from "./sessions.json";

type Loose<T> = T extends string
  ? string
  : T extends number | boolean | null | undefined
    ? T
    : T extends readonly (infer U)[]
      ? Loose<U>[]
      : T extends object
        ? { [K in keyof T]: Loose<T[K]> }
        : T;

export const checked = {
  quick: quick satisfies Loose<QuickProfileList>,
  selection: selection satisfies Loose<SelectionResult>,
  profiles: profiles satisfies Loose<ProfileResult>,
  profile: profile satisfies Loose<SeriesProfile>,
  projection: projection satisfies Loose<LLMProfileView>,
  sessions: sessions satisfies Loose<SessionPage>,
  availability: availability satisfies Loose<AvailabilityList>,
  override: override.override satisfies Loose<ProfileOverrideRecord>,
};
