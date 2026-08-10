<#import "template.ftl" as layout>
<@layout.registrationLayout displayMessage=false; section>
<#-- The portal's "Forgot password?" panel, as a Keycloak screen. Keycloak sends
     a tokenised reset link by email, which the portal's own implementation did
     not do. -->
<#if section = "form">
  <div class="gentian-login__card">
    <div class="gentian-login__logo" role="img" aria-label="Gentian"></div>
    <h1 class="gentian-login__title">gentian</h1>
    <p class="gentian-login__subtitle">
      Enter your email and we will send you a link to reset your password.
    </p>

    <#if message?has_content>
      <p class="gentian-login__${(message.type = 'error')?then('error','info')}">
        ${kcSanitize(message.summary)?no_esc}
      </p>
    </#if>

    <form id="kc-reset-password-form" class="gentian-login__form"
          action="${url.loginAction}" method="post">
      <div>
        <label class="gentian-login__label" for="username">
          <#if !realm.loginWithEmailAllowed>${msg("username")}
          <#elseif !realm.registrationEmailAsUsername>${msg("usernameOrEmail")}
          <#else>${msg("email")}</#if>
        </label>
        <input id="username" name="username" class="gentian-login__input"
               type="text" autofocus autocomplete="username"
               value="${(auth.attemptedUsername!'')}"
               placeholder="you@example.org"/>
      </div>

      <button class="gentian-login__btn" type="submit">${msg("doSubmit")}</button>
    </form>

    <a class="gentian-login__link" href="${url.loginUrl}">${msg("backToLogin")?no_esc}</a>
  </div>
</#if>
</@layout.registrationLayout>
