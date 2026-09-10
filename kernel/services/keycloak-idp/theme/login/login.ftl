<#import "template.ftl" as layout>
<@layout.registrationLayout displayMessage=false; section>
<#-- Reproduces gentian-ui's LoginPage card. displayMessage=false because the
     card renders messages itself, in the portal's own error/info styling,
     rather than in Keycloak's default alert block. -->
<#if section = "form">
  <div class="gentian-login__card">
    <div class="gentian-login__logo" role="img" aria-label="Gentian"></div>
    <h1 class="gentian-login__title">gentian</h1>
    <p class="gentian-login__subtitle">Sign in to your workspace</p>

    <#if message?has_content>
      <p class="gentian-login__${(message.type = 'error')?then('error','info')}">
        ${kcSanitize(message.summary)?no_esc}
      </p>
    </#if>

    <#if realm.password>
      <form id="kc-form-login" class="gentian-login__form"
            action="${url.loginAction}" method="post">
        <div>
          <label class="gentian-login__label" for="username">
            <#if !realm.loginWithEmailAllowed>${msg("username")}
            <#elseif !realm.registrationEmailAsUsername>${msg("usernameOrEmail")}
            <#else>${msg("email")}</#if>
          </label>
          <input id="username" name="username" class="gentian-login__input"
                 type="text" autofocus autocomplete="username"
                 value="${(login.username!'')}"
                 placeholder="you@example.org"
                 aria-invalid="<#if messagesPerField.existsError('username','password')>true</#if>"/>
        </div>

        <div>
          <label class="gentian-login__label" for="password">${msg("password")}</label>
          <input id="password" name="password" class="gentian-login__input"
                 type="password" autocomplete="current-password"
                 aria-invalid="<#if messagesPerField.existsError('username','password')>true</#if>"/>
        </div>

        <#-- Carries the auth session through; without it the POST is rejected. -->
        <input type="hidden" id="id-hidden-input" name="credentialId"
               <#if auth.selectedCredential?has_content>value="${auth.selectedCredential}"</#if>/>

        <button class="gentian-login__btn" name="login" id="kc-login" type="submit"
                <#if usernameHidden??>disabled</#if>>${msg("doLogIn")}</button>
      </form>

      <#if realm.resetPasswordAllowed>
        <a class="gentian-login__link" href="${url.loginResetCredentialsUrl}">
          ${msg("doForgotPassword")}
        </a>
      </#if>
    </#if>
  </div>
</#if>
</@layout.registrationLayout>
