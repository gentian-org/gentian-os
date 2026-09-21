-- =============================================================================
-- Gentian App Store — presentation and commercial metadata
--
-- Scope: tenant apps only. System services and shared apps are provisioned by
-- the cluster administrator through the Director and never appear here, so the
-- store has no notion of a component class. Ingest rejects any catalogue entry
-- whose ComponentProfile cannot be deployed as class "tenant".
--
-- Holds what was removed from ComponentProfile, plus the commercial data that
-- never belonged in a cluster.
--
-- Boundary (D11, operator-split-plan §3.6): the store may TRIGGER, it may not
-- SUPPLY. It calls the director's ingestion endpoint on the kernel gateway,
-- bearer only. The director then materialises the profile bundle at the
-- requested digest from the catalogue git repository — never from this
-- database. So a request crosses the boundary; the technical spec never does.
--
-- Join key throughout: (catalogue.slug, app.name, app_version.version)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Sources and access
-- -----------------------------------------------------------------------------

create table catalogue (
    id           bigint generated always as identity primary key,
    slug         text        not null unique,
    display_name text        not null,
    is_public    boolean     not null default false,
    created_at   timestamptz not null default now(),
    constraint catalogue_slug_fmt check (slug ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$')
);

create table cluster (
    id            bigint generated always as identity primary key,
    slug          text        not null unique,
    display_name  text        not null,
    public_key    bytea       not null,   -- authenticates this cluster's API calls
    registered_at timestamptz not null default now(),
    disabled_at   timestamptz
);

-- A non-public catalogue is visible only to clusters listed here. This is what
-- keeps partner and private catalogues out of the open.
create table catalogue_access (
    catalogue_id bigint      not null references catalogue(id) on delete cascade,
    cluster_id   bigint      not null references cluster(id)   on delete cascade,
    granted_at   timestamptz not null default now(),
    primary key (catalogue_id, cluster_id)
);

create table vendor (
    id            bigint generated always as identity primary key,
    slug          text not null unique,
    legal_name    text not null,
    homepage_url  text,
    support_url   text,
    contact_email text
);

-- -----------------------------------------------------------------------------
-- App identity and versions
-- -----------------------------------------------------------------------------

create table app (
    id           bigint generated always as identity primary key,
    catalogue_id bigint not null references catalogue(id) on delete cascade,
    -- Matches ComponentProfile metadata.name exactly. Opaque: never parsed for
    -- family or edition, which are columns precisely so the name need not carry
    -- them.
    name         text   not null,
    family       text   not null,
    vendor_id    bigint not null references vendor(id),
    license_spdx text,
    homepage_url text,
    source_url   text,
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    unique (catalogue_id, name),
    constraint app_name_dns
        check (name ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$' and char_length(name) <= 40)
);

-- Lookups rather than enums: a new edition or review level is a row, not a
-- migration plus a coordinated deploy.
create table edition (
    code         text primary key,      -- ce, me, ee
    display_name text not null,
    rank         int  not null
);

create table trust_tier (
    code         text primary key,      -- platform, certified, experimental
    display_name text not null,
    rank         int  not null
);

create table app_version (
    id              bigint generated always as identity primary key,
    app_id    bigint not null references app(id) on delete cascade,
    version         text   not null,   -- catalogue entry version
    upstream_version text,             -- the app's own version, display only
    edition_code    text   not null references edition(code),
    trust_tier_code text   not null references trust_tier(code),
    -- The digest the director requests when it materialises this entry from
    -- the catalogue repository, and annotates onto the resulting CR. Not a
    -- second hash that happens to agree: the same value, so a store row and a
    -- git bundle are provably the same artifact.
    profile_digest  text   not null,
    released_at     timestamptz not null default now(),
    yanked_at       timestamptz,       -- soft withdrawal; never delete a version
    unique (app_id, version)
);

create index on app_version (app_id, released_at desc);

-- -----------------------------------------------------------------------------
-- Localised text
--
-- Translation tables rather than JSON columns: a missing translation is then a
-- missing row that a query can fall back on, instead of a null buried inside a
-- document that every reader has to handle.
-- -----------------------------------------------------------------------------

create table locale (
    code text primary key              -- BCP 47: en, de, de-CH, fr
);

-- Stable across versions: what the app is.
create table app_text (
    app_id   bigint not null references app(id) on delete cascade,
    locale_code    text   not null references locale(code),
    display_name   text   not null,
    summary        text   not null,    -- one line, for cards and search results
    description_md text,               -- long form; sanitised on ingest, not on render
    primary key (app_id, locale_code),
    constraint display_name_len check (char_length(display_name) between 1 and 80),
    constraint summary_len      check (char_length(summary) between 1 and 200),
    constraint description_len  check (description_md is null or char_length(description_md) <= 20000)
);

-- Per version: what changed.
create table app_version_text (
    app_version_id bigint not null references app_version(id) on delete cascade,
    locale_code          text   not null references locale(code),
    release_notes_md     text,
    primary key (app_version_id, locale_code),
    constraint release_notes_len check (release_notes_md is null or char_length(release_notes_md) <= 20000)
);

-- -----------------------------------------------------------------------------
-- Media
-- -----------------------------------------------------------------------------

create type media_kind as enum ('icon', 'logo', 'screenshot', 'video');

create table app_media (
    id           bigint generated always as identity primary key,
    app_id bigint     not null references app(id) on delete cascade,
    kind         media_kind not null,
    -- Locale-specific screenshots (a UI shown in German) are common; null means
    -- it serves every locale.
    locale_code  text references locale(code),
    -- Either a stored object, or a slug into the shared Gentian icon set.
    uri          text,
    icon_slug    text,
    content_type text,
    width_px     int,
    height_px    int,
    alt_text     text,
    sort_order   int not null default 0,
    constraint media_one_source check (num_nonnulls(uri, icon_slug) = 1),
    constraint media_icon_slug_fmt
        check (icon_slug is null or icon_slug ~ '^[a-z][a-z0-9-]*$'),
    constraint media_alt_required
        check (kind not in ('screenshot', 'video') or alt_text is not null)
);

create unique index app_one_icon on app_media (app_id) where kind = 'icon';
create unique index app_one_logo on app_media (app_id) where kind = 'logo';
create index on app_media (app_id, kind, sort_order);

-- -----------------------------------------------------------------------------
-- Classification
-- -----------------------------------------------------------------------------

create table category (
    slug        text primary key,
    parent_slug text references category(slug),
    sort_order  int not null default 0,
    constraint category_not_own_parent check (parent_slug is distinct from slug)
);

create table category_text (
    category_slug text not null references category(slug) on delete cascade,
    locale_code   text not null references locale(code),
    display_name  text not null,
    primary key (category_slug, locale_code)
);

create table app_category (
    app_id  bigint  not null references app(id) on delete cascade,
    category_slug text    not null references category(slug),
    is_primary    boolean not null default false,
    primary key (app_id, category_slug)
);

create unique index app_one_primary_category
    on app_category (app_id) where is_primary;

create table app_keyword (
    app_id bigint not null references app(id) on delete cascade,
    keyword      text   not null,
    primary key (app_id, keyword),
    constraint keyword_fmt check (keyword ~ '^[a-z0-9][a-z0-9 -]{0,38}[a-z0-9]$')
);

-- -----------------------------------------------------------------------------
-- Portal tiles — the presentation half only
--
-- A tile's existence, link suffix, link target and allowed group are routing
-- and authorization. They stay in ComponentProfile.expose in the cluster and
-- must never be served from here: a store that can rename a tile is cosmetic,
-- a store that can change who may see it is a privilege escalation path.
-- tile_name is the join back to the profile's expose entry.
-- -----------------------------------------------------------------------------

create table app_tile_text (
    app_id bigint not null references app(id) on delete cascade,
    tile_name    text   not null,
    locale_code  text   not null references locale(code),
    display_name text   not null,
    primary key (app_id, tile_name, locale_code),
    constraint tile_display_name_len check (char_length(display_name) between 1 and 60)
);

create table app_tile_media (
    app_id bigint not null references app(id) on delete cascade,
    tile_name    text   not null,
    uri          text,
    icon_slug    text,
    primary key (app_id, tile_name),
    constraint tile_one_source check (num_nonnulls(uri, icon_slug) = 1)
);

-- -----------------------------------------------------------------------------
-- Commercial
-- -----------------------------------------------------------------------------

create table tenant (
    id         bigint generated always as identity primary key,
    cluster_id bigint not null references cluster(id) on delete cascade,
    name       text   not null,        -- tenant name inside that cluster
    created_at timestamptz not null default now(),
    unique (cluster_id, name)
);

create table plan (
    id             bigint generated always as identity primary key,
    -- Null app_id means a bundle; members live in plan_app.
    app_id   bigint references app(id) on delete cascade,
    slug           text    not null,
    billing_period text    not null,
    seat_based     boolean not null default false,
    published_at   timestamptz,
    retired_at     timestamptz,
    unique (app_id, slug),
    constraint plan_period check (billing_period in ('monthly', 'yearly', 'perpetual'))
);

create table plan_app (
    plan_id      bigint not null references plan(id) on delete cascade,
    app_id bigint not null references app(id) on delete cascade,
    primary key (plan_id, app_id)
);

create table plan_text (
    plan_id      bigint not null references plan(id) on delete cascade,
    locale_code  text   not null references locale(code),
    display_name text   not null,
    summary      text,
    primary key (plan_id, locale_code)
);

-- Prices are append-only and time-bounded: an invoice must be reproducible from
-- the row that was current when it was issued, so a price is never updated.
create table plan_price (
    id           bigint generated always as identity primary key,
    plan_id      bigint      not null references plan(id) on delete cascade,
    currency     char(3)     not null,
    amount_minor bigint      not null,
    valid_from   timestamptz not null,
    valid_until  timestamptz,
    constraint price_nonneg check (amount_minor >= 0),
    constraint price_window check (valid_until is null or valid_until > valid_from)
);

create index on plan_price (plan_id, valid_from desc);

create table subscription (
    id           bigint generated always as identity primary key,
    tenant_id    bigint      not null references tenant(id) on delete cascade,
    plan_id      bigint      not null references plan(id),
    seats        int,
    starts_at    timestamptz not null,
    ends_at      timestamptz,
    cancelled_at timestamptz,
    constraint sub_seats check (seats is null or seats > 0),
    constraint sub_window check (ends_at is null or ends_at > starts_at)
);

create index on subscription (tenant_id, starts_at desc);
create index on subscription (plan_id);

-- -----------------------------------------------------------------------------
-- Entitlement: the store's record of a commercial fact, signed
--
-- The cluster does NOT cache this and check it at install time. Per security
-- principle 9 the in-cluster representation is data in git plus an OpenFGA
-- tuple, written by the director:
--
--     type catalogue_entry
--         define entitled: [tenant]
--         define can_install: entitled
--
-- Flow: the store issues the grant below; the director verifies the signature,
-- commits the record, and writes the tuple. expires_at becomes a TTL condition
-- on that tuple (principle 5), not an offline signature check at install time.
-- The signature exists so the director can trust the store, not so a cluster
-- can decide without one.
--
-- The signature covers the FACT only — (tenant, app, version, digest,
-- issued_at, expires_at). A private catalogue's fetch token and the pull
-- credential for chart and images travel in the same request to the director
-- but are never part of the signed record: the record lands in git, which may
-- be public, and the credentials land in OpenBao through the credential
-- manager, written as the tenant admin (ui-restructure.md §3). This table
-- therefore stores no credential either; it records that one was issued.
--
-- Provisional, pending this flow: cluster.public_key and entitlement_check
-- describe a cluster-asks-store protocol that the flow above makes
-- unnecessary. They stay only if a cluster ever has to CALL the store — seat
-- or usage reporting — which entitlement does not need.
-- -----------------------------------------------------------------------------

create table signing_key (
    key_id      text primary key,
    public_key  bytea       not null,
    created_at  timestamptz not null default now(),
    retired_at  timestamptz
);

create table entitlement_grant (
    id              bigint generated always as identity primary key,
    tenant_id       bigint      not null references tenant(id) on delete cascade,
    app_id    bigint      not null references app(id),
    subscription_id bigint      references subscription(id),
    granted         boolean     not null,
    reason          text,
    seats           int,
    issued_at       timestamptz not null default now(),
    expires_at      timestamptz not null,
    key_id          text        not null references signing_key(key_id),
    signature       bytea       not null,
    revoked_at      timestamptz,
    constraint grant_window check (expires_at > issued_at),
    -- A denial carries a reason; silence is not an answer a cluster can log.
    constraint grant_denial_has_reason check (granted or reason is not null)
);

create index on entitlement_grant (tenant_id, app_id, issued_at desc);
create index on entitlement_grant (expires_at) where revoked_at is null;

-- Append-only audit of every check, for billing disputes and abuse detection.
-- Partition by month once volume justifies it.
create table entitlement_check (
    id           bigint generated always as identity primary key,
    cluster_id   bigint      not null references cluster(id),
    tenant_id    bigint      references tenant(id),
    app_id bigint      references app(id),
    requested_at timestamptz not null default now(),
    result       text        not null,
    grant_id     bigint      references entitlement_grant(id)
);

create index on entitlement_check (cluster_id, requested_at desc);

-- -----------------------------------------------------------------------------
-- Reading with locale fallback
--
-- $1 requested locale, $2 fallback (typically 'en').
-- -----------------------------------------------------------------------------

-- select c.name,
--        coalesce(t.display_name, f.display_name) as display_name,
--        coalesce(t.summary,      f.summary)      as summary
--   from app c
--   left join app_text t on t.app_id = c.id and t.locale_code = $1
--   left join app_text f on f.app_id = c.id and f.locale_code = $2
--  where c.catalogue_id = $3;
